package withdrawal

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/amount"
	"github.com/xiaoqi/mini-custody/backend/internal/store/postgres"
)

var ErrInvalidRequest = errors.New("提币请求参数无效")

// Chain 定义提币费用估算、签名准备、广播和确认所需的链访问能力。
type Chain interface {
	BlockNumber(ctx context.Context) (uint64, error)
	HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
	BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}

// CreationStore 定义创建提币需要的钱包查询和余额占用能力。
type CreationStore interface {
	WalletAddressByUser(ctx context.Context, userID int64) (postgres.WalletAddress, error)
	WithdrawalByIdempotencyKey(ctx context.Context, userID int64, idempotencyKey string) (postgres.Withdrawal, error)
	ReserveWithdrawal(ctx context.Context, request postgres.WithdrawalRequest) (postgres.Withdrawal, bool, error)
}

// CreateRequest 描述来自 API 层的严格提币创建参数。
type CreateRequest struct {
	IdempotencyKey string
	UserID         int64
	ToAddress      string
	AmountETH      string
}

// FeeQuote 描述当前 EIP-1559 费用参数和最大预留网络费。
type FeeQuote struct {
	GasLimit                uint64
	MaxFeePerGasWei         *big.Int
	MaxPriorityFeePerGasWei *big.Int
	ReservedFeeWei          *big.Int
}

// CreateResult 返回提币记录、费用估算和是否新建。
type CreateResult struct {
	Withdrawal postgres.Withdrawal
	Fee        FeeQuote
	Created    bool
}

// QuoteRequest 描述只读提币费用估算参数。
type QuoteRequest struct {
	UserID    int64
	ToAddress string
	AmountETH string
}

// QuoteResult 返回精确提币金额和当前最大费用估算。
type QuoteResult struct {
	AmountWei *big.Int
	Fee       FeeQuote
}

// Service 提供创建提币和费用估算能力。
type Service struct {
	chain Chain
	store CreationStore
}

// NewService 创建提币业务服务。
func NewService(chain Chain, store CreationStore) (*Service, error) {
	if chain == nil || store == nil {
		return nil, errors.New("提币服务依赖不能为空")
	}
	return &Service{chain: chain, store: store}, nil
}

// Create 校验 ETH 金额、估算最大费用并幂等占用用户余额。
func (s *Service) Create(ctx context.Context, request CreateRequest) (CreateResult, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.IdempotencyKey == "" {
		return CreateResult{}, fmt.Errorf("%w：必须提供幂等标识", ErrInvalidRequest)
	}
	walletAddress, value, to, err := s.validateQuoteInput(ctx, QuoteRequest{
		UserID: request.UserID, ToAddress: request.ToAddress, AmountETH: request.AmountETH,
	})
	if err != nil {
		return CreateResult{}, err
	}
	existing, err := s.store.WithdrawalByIdempotencyKey(ctx, request.UserID, request.IdempotencyKey)
	if err == nil {
		if existing.AddressID != walletAddress.ID || existing.ToAddress != strings.ToLower(to.Hex()) || existing.AmountWei.Cmp(value) != 0 {
			return CreateResult{}, postgres.ErrIdempotencyConflict
		}
		quote, err := quoteForExisting(ctx, s.chain, walletAddress, existing)
		if err != nil {
			return CreateResult{}, err
		}
		return CreateResult{Withdrawal: existing, Fee: quote, Created: false}, nil
	}
	if !errors.Is(err, postgres.ErrNotFound) {
		return CreateResult{}, fmt.Errorf("查询幂等提币失败：%w", err)
	}
	quote, err := estimateFee(ctx, s.chain, common.HexToAddress(walletAddress.Address), to, value)
	if err != nil {
		return CreateResult{}, err
	}
	item, created, err := s.store.ReserveWithdrawal(ctx, postgres.WithdrawalRequest{
		IdempotencyKey: request.IdempotencyKey,
		UserID:         request.UserID,
		AddressID:      walletAddress.ID,
		ToAddress:      to.Hex(),
		AmountWei:      value,
		ReservedFeeWei: quote.ReservedFeeWei,
	})
	if err != nil {
		return CreateResult{}, err
	}
	quote.ReservedFeeWei = new(big.Int).Set(item.ReservedFeeWei)
	return CreateResult{Withdrawal: item, Fee: quote, Created: created}, nil
}

// Quote 校验提币参数并返回不写数据库的当前最大费用估算。
func (s *Service) Quote(ctx context.Context, request QuoteRequest) (QuoteResult, error) {
	walletAddress, value, to, err := s.validateQuoteInput(ctx, request)
	if err != nil {
		return QuoteResult{}, err
	}
	quote, err := estimateFee(ctx, s.chain, common.HexToAddress(walletAddress.Address), to, value)
	if err != nil {
		return QuoteResult{}, err
	}
	return QuoteResult{AmountWei: value, Fee: quote}, nil
}

// validateQuoteInput 校验用户输入并查询托管地址，不提前发起链上费用估算。
func (s *Service) validateQuoteInput(ctx context.Context, request QuoteRequest) (postgres.WalletAddress, *big.Int, common.Address, error) {
	if request.UserID <= 0 {
		return postgres.WalletAddress{}, nil, common.Address{}, fmt.Errorf("%w：用户 ID 无效", ErrInvalidRequest)
	}
	if !common.IsHexAddress(strings.TrimSpace(request.ToAddress)) {
		return postgres.WalletAddress{}, nil, common.Address{}, fmt.Errorf("%w：提币目标地址无效", ErrInvalidRequest)
	}
	value, err := amount.ParseETH(request.AmountETH)
	if err != nil {
		return postgres.WalletAddress{}, nil, common.Address{}, fmt.Errorf("%w，金额解析失败：%v", ErrInvalidRequest, err)
	}
	if err := amount.RequirePositive(value); err != nil {
		return postgres.WalletAddress{}, nil, common.Address{}, fmt.Errorf("%w，金额必须大于零：%v", ErrInvalidRequest, err)
	}
	walletAddress, err := s.store.WalletAddressByUser(ctx, request.UserID)
	if err != nil {
		return postgres.WalletAddress{}, nil, common.Address{}, fmt.Errorf("查询用户托管地址失败：%w", err)
	}
	to := common.HexToAddress(request.ToAddress)
	return walletAddress, value, to, nil
}

// quoteForExisting 优先使用原提币已持久化的费用参数，避免幂等重试依赖当前链上余额。
func quoteForExisting(ctx context.Context, chain Chain, address postgres.WalletAddress, item postgres.Withdrawal) (FeeQuote, error) {
	if item.GasLimit != nil && *item.GasLimit > 0 && item.MaxFeePerGasWei != nil && item.MaxPriorityFeePerGasWei != nil {
		return FeeQuote{
			GasLimit:                uint64(*item.GasLimit),
			MaxFeePerGasWei:         new(big.Int).Set(item.MaxFeePerGasWei),
			MaxPriorityFeePerGasWei: new(big.Int).Set(item.MaxPriorityFeePerGasWei),
			ReservedFeeWei:          new(big.Int).Set(item.ReservedFeeWei),
		}, nil
	}
	quote, err := estimateFee(ctx, chain, common.HexToAddress(address.Address), common.HexToAddress(item.ToAddress), item.AmountWei)
	if err != nil {
		return FeeQuote{}, err
	}
	quote.ReservedFeeWei = new(big.Int).Set(item.ReservedFeeWei)
	return quote, nil
}

// estimateFee 根据最新 Base Fee、建议 Tip 和 Gas 估算计算最大预留费用。
func estimateFee(ctx context.Context, chain Chain, from, to common.Address, value *big.Int) (FeeQuote, error) {
	header, err := chain.HeaderByNumber(ctx, nil)
	if err != nil {
		return FeeQuote{}, fmt.Errorf("查询最新区块费用失败：%w", err)
	}
	if header == nil || header.BaseFee == nil || header.BaseFee.Sign() < 0 {
		return FeeQuote{}, errors.New("最新区块缺少有效 Base Fee")
	}
	tip, err := chain.SuggestGasTipCap(ctx)
	if err != nil {
		return FeeQuote{}, fmt.Errorf("查询建议优先费失败：%w", err)
	}
	if tip == nil || tip.Sign() < 0 {
		return FeeQuote{}, errors.New("RPC 返回的建议优先费无效")
	}
	gasLimit, err := chain.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &to, Value: value})
	if err != nil {
		return FeeQuote{}, fmt.Errorf("估算提币 Gas 失败：%w", err)
	}
	if gasLimit == 0 {
		return FeeQuote{}, errors.New("RPC 返回的 Gas Limit 无效")
	}
	maxFee := new(big.Int).Add(new(big.Int).Mul(header.BaseFee, big.NewInt(2)), tip)
	reserved := new(big.Int).Mul(new(big.Int).SetUint64(gasLimit), maxFee)
	return FeeQuote{
		GasLimit:                gasLimit,
		MaxFeePerGasWei:         maxFee,
		MaxPriorityFeePerGasWei: new(big.Int).Set(tip),
		ReservedFeeWei:          reserved,
	}, nil
}
