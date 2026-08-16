package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

var ErrInvalidRequest = errors.New("Token 提币请求无效")

// FeeChain 定义 Token 提币费用试算所需的最小 EVM 能力。
type FeeChain interface {
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
}

// QuoteContract 定义 Token 提币试算所需的合约能力。
type QuoteContract interface {
	Address() common.Address
	EstimateTransferGas(ctx context.Context, from, to common.Address, amountUnits *big.Int) (uint64, error)
}

// ServiceStore 定义 Token 提币服务所需的查询和余额占用能力。
type ServiceStore interface {
	TokenWithdrawalByIdempotencyKey(ctx context.Context, userID int64, idempotencyKey string) (postgres.TokenWithdrawal, error)
	PlatformWalletByRole(ctx context.Context, network, role string) (postgres.PlatformWallet, error)
	ReserveTokenWithdrawal(ctx context.Context, request postgres.TokenWithdrawalRequest) (postgres.TokenWithdrawal, bool, error)
}

// FeeQuote 描述 Token transfer 的 EIP-1559 最大网络费试算。
type FeeQuote struct {
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	ReservedFeeWei          *big.Int
}

// QuoteRequest 描述 Token 提币试算输入。
type QuoteRequest struct {
	UserID    int64
	ToAddress string
	Amount    string
}

// QuoteResult 描述 Token 金额和平台预计承担的 ETH Gas。
type QuoteResult struct {
	AmountUnits *big.Int
	Fee         FeeQuote
}

// CreateRequest 描述幂等创建 Token 提币的输入。
type CreateRequest struct {
	IdempotencyKey string
	UserID         int64
	ToAddress      string
	Amount         string
}

// CreateResult 描述 Token 提币创建结果及对应费用试算。
type CreateResult struct {
	Withdrawal postgres.TokenWithdrawal
	Fee        FeeQuote
	Created    bool
}

// Service 负责 Token 金额校验、费用试算和用户余额占用。
type Service struct {
	chain    FeeChain
	contract QuoteContract
	store    ServiceStore
	asset    postgres.Asset
}

// NewService 创建并校验 Token 提币服务。
func NewService(chain FeeChain, contract QuoteContract, store ServiceStore, asset postgres.Asset) (*Service, error) {
	if chain == nil || contract == nil || store == nil {
		return nil, errors.New("Token 提币服务依赖不能为空")
	}
	if asset.ID <= 0 || asset.Network != postgres.NetworkSepolia || asset.AssetType != postgres.AssetTypeERC20 ||
		asset.Decimals == 0 || asset.Decimals > 18 || !asset.Enabled ||
		!common.IsHexAddress(asset.ContractAddress) || common.HexToAddress(asset.ContractAddress) != contract.Address() {
		return nil, errors.New("Token 提币资产配置无效")
	}
	return &Service{chain: chain, contract: contract, store: store, asset: asset}, nil
}

// Quote 校验 Token 提币输入并返回不写数据库的当前最大 Gas 估算。
func (s *Service) Quote(ctx context.Context, request QuoteRequest) (QuoteResult, error) {
	amountUnits, to, platform, err := s.validate(ctx, request.UserID, request.ToAddress, request.Amount)
	if err != nil {
		return QuoteResult{}, err
	}
	fee, err := estimateFee(ctx, s.chain, s.contract, common.HexToAddress(platform.Address), to, amountUnits)
	if err != nil {
		return QuoteResult{}, err
	}
	return QuoteResult{AmountUnits: amountUnits, Fee: fee}, nil
}

// Create 幂等创建 Token 提币并原子占用用户 Token 余额。
func (s *Service) Create(ctx context.Context, request CreateRequest) (CreateResult, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" {
		return CreateResult{}, fmt.Errorf("%w：必须提供幂等标识", ErrInvalidRequest)
	}
	amountUnits, to, platform, err := s.validate(ctx, request.UserID, request.ToAddress, request.Amount)
	if err != nil {
		return CreateResult{}, err
	}
	normalizedTo := strings.ToLower(to.Hex())
	existing, err := s.store.TokenWithdrawalByIdempotencyKey(ctx, request.UserID, request.IdempotencyKey)
	if err == nil {
		if existing.AssetID != s.asset.ID || existing.PlatformWalletID != platform.ID ||
			existing.ToAddress != normalizedTo || existing.AmountUnits.Cmp(amountUnits) != 0 {
			return CreateResult{}, postgres.ErrIdempotencyConflict
		}
		fee, err := s.quoteForExisting(ctx, platform, existing)
		if err != nil {
			return CreateResult{}, err
		}
		return CreateResult{Withdrawal: existing, Fee: fee}, nil
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return CreateResult{}, fmt.Errorf("查询幂等 Token 提币失败：%w", err)
	}
	fee, err := estimateFee(ctx, s.chain, s.contract, common.HexToAddress(platform.Address), to, amountUnits)
	if err != nil {
		return CreateResult{}, err
	}
	item, created, err := s.store.ReserveTokenWithdrawal(ctx, postgres.TokenWithdrawalRequest{
		IdempotencyKey: request.IdempotencyKey, UserID: request.UserID, AssetID: s.asset.ID,
		PlatformWalletID: platform.ID, ToAddress: normalizedTo, AmountUnits: amountUnits,
	})
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Withdrawal: item, Fee: fee, Created: created}, nil
}

// validate 校验用户输入、解析最小单位并查询平台热钱包。
func (s *Service) validate(ctx context.Context, userID int64, toAddress, value string) (*big.Int, common.Address, postgres.PlatformWallet, error) {
	if userID <= 0 {
		return nil, common.Address{}, postgres.PlatformWallet{}, fmt.Errorf("%w：用户 ID 无效", ErrInvalidRequest)
	}
	toAddress = strings.TrimSpace(toAddress)
	if !common.IsHexAddress(toAddress) {
		return nil, common.Address{}, postgres.PlatformWallet{}, fmt.Errorf("%w：提币目标地址无效", ErrInvalidRequest)
	}
	to := common.HexToAddress(toAddress)
	if to == (common.Address{}) || to == s.contract.Address() {
		return nil, common.Address{}, postgres.PlatformWallet{}, fmt.Errorf("%w：提币目标地址不能为零地址或 Token 合约", ErrInvalidRequest)
	}
	amountUnits, err := amount.ParseDecimal(value, s.asset.Decimals)
	if err != nil || amountUnits.Sign() <= 0 {
		return nil, common.Address{}, postgres.PlatformWallet{}, fmt.Errorf("%w：Token 金额必须大于零且最多 %d 位小数", ErrInvalidRequest, s.asset.Decimals)
	}
	platform, err := s.store.PlatformWalletByRole(ctx, postgres.NetworkSepolia, postgres.PlatformRoleHot)
	if err != nil {
		return nil, common.Address{}, postgres.PlatformWallet{}, fmt.Errorf("查询 Token 提币平台热钱包失败：%w", err)
	}
	return amountUnits, to, platform, nil
}

// quoteForExisting 优先返回已签名提币持久化的 Gas 参数，保证幂等重试稳定。
func (s *Service) quoteForExisting(ctx context.Context, platform postgres.PlatformWallet, item postgres.TokenWithdrawal) (FeeQuote, error) {
	if item.GasLimit != nil && *item.GasLimit > 0 && item.MaxFeePerGasWei != nil && item.MaxPriorityFeePerGasWei != nil {
		gasLimit := uint64(*item.GasLimit)
		return FeeQuote{
			GasLimit: gasLimit, MaxFeePerGasWei: new(big.Int).Set(item.MaxFeePerGasWei),
			MaxPriorityFeePerGasWei: new(big.Int).Set(item.MaxPriorityFeePerGasWei),
			ReservedFeeWei:          new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), item.MaxFeePerGasWei),
		}, nil
	}
	return estimateFee(ctx, s.chain, s.contract, common.HexToAddress(platform.Address),
		common.HexToAddress(item.ToAddress), item.AmountUnits)
}

// estimateFee 根据 transfer Gas、最新 Base Fee 和建议 Tip 计算平台最大 Gas 成本。
func estimateFee(ctx context.Context, chain FeeChain, contract QuoteContract, from, to common.Address, amountUnits *big.Int) (FeeQuote, error) {
	gasLimit, err := contract.EstimateTransferGas(ctx, from, to, amountUnits)
	if err != nil {
		return FeeQuote{}, fmt.Errorf("估算 Token 提币 Gas 失败：%w", err)
	}
	if gasLimit == 0 {
		return FeeQuote{}, errors.New("RPC 返回的 Token 提币 Gas Limit 无效")
	}
	header, err := chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return FeeQuote{}, fmt.Errorf("查询 Token 提币最新区块费用失败：%w", err)
	}
	if header == nil || header.BaseFee == nil || header.BaseFee.Sign() < 0 {
		return FeeQuote{}, errors.New("Token 提币最新区块缺少有效 Base Fee")
	}
	tip, err := chain.SuggestGasTipCap(ctx)
	if err != nil {
		return FeeQuote{}, fmt.Errorf("查询 Token 提币建议优先费失败：%w", err)
	}
	if tip == nil || tip.Sign() < 0 {
		return FeeQuote{}, errors.New("RPC 返回的 Token 提币建议优先费无效")
	}
	maxFee := new(big.Int).Add(new(big.Int).Mul(header.BaseFee, big.NewInt(2)), tip)
	reserved := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), maxFee)
	return FeeQuote{GasLimit: gasLimit, MaxFeePerGasWei: maxFee,
		MaxPriorityFeePerGasWei: new(big.Int).Set(tip), ReservedFeeWei: reserved}, nil
}
