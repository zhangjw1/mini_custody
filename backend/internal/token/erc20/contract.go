package erc20

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

var (
	ErrContractCodeMissing = errors.New("ERC-20 合约地址没有部署字节码")
	ErrMetadataMismatch    = errors.New("ERC-20 链上元数据与配置不一致")
	ErrTransferRejected    = errors.New("ERC-20 transfer 返回失败")
	ErrInvalidTransferLog  = errors.New("ERC-20 Transfer Event 数据无效")
	ErrRemovedLog          = errors.New("ERC-20 Transfer Event 已被链重组移除")
)

// Chain 定义 ERC-20 合约适配器所需的最小 EVM RPC 能力。
type Chain interface {
	CodeAt(ctx context.Context, contract common.Address, block *big.Int) ([]byte, error)
	CallContract(ctx context.Context, call ethereum.CallMsg, block *big.Int) ([]byte, error)
	FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error)
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
}

// Metadata 描述 ERC-20 合约的链上元数据。
type Metadata struct {
	Symbol   string
	Decimals uint8
}

// TransferEvent 描述经过严格校验的 ERC-20 Transfer Event。
type TransferEvent struct {
	Contract    common.Address
	From        common.Address
	To          common.Address
	AmountUnits *big.Int
	TxHash      common.Hash
	LogIndex    uint
	BlockNumber uint64
	BlockHash   common.Hash
}

// Contract 提供单个标准 ERC-20 合约的结构化调用和 Event 解码能力。
type Contract struct {
	chain   Chain
	address common.Address
	abi     abi.ABI
}

// NewContract 创建并校验 ERC-20 合约适配器依赖。
func NewContract(chain Chain, address common.Address) (*Contract, error) {
	if chain == nil {
		return nil, errors.New("必须提供 ERC-20 链客户端")
	}
	if address == (common.Address{}) {
		return nil, errors.New("必须提供非零 ERC-20 合约地址")
	}
	contractABI, err := StandardABI()
	if err != nil {
		return nil, err
	}
	return &Contract{chain: chain, address: address, abi: contractABI}, nil
}

// Address 返回当前适配器绑定的 ERC-20 合约地址。
func (c *Contract) Address() common.Address {
	return c.address
}

// Validate 校验合约代码、symbol 和 decimals 与启动配置一致。
func (c *Contract) Validate(ctx context.Context, expectedSymbol string, expectedDecimals uint8) (Metadata, error) {
	code, err := c.chain.CodeAt(ctx, c.address, nil)
	if err != nil {
		return Metadata{}, fmt.Errorf("查询 ERC-20 合约代码失败：%w", err)
	}
	if len(code) == 0 {
		return Metadata{}, ErrContractCodeMissing
	}
	metadata, err := c.Metadata(ctx)
	if err != nil {
		return Metadata{}, err
	}
	if metadata.Symbol != strings.TrimSpace(expectedSymbol) || metadata.Decimals != expectedDecimals {
		return Metadata{}, fmt.Errorf("%w：链上 symbol=%s、decimals=%d", ErrMetadataMismatch, metadata.Symbol, metadata.Decimals)
	}
	return metadata, nil
}

// Metadata 查询 ERC-20 合约的 symbol 和 decimals。
func (c *Contract) Metadata(ctx context.Context) (Metadata, error) {
	symbol, err := c.Symbol(ctx)
	if err != nil {
		return Metadata{}, err
	}
	decimals, err := c.Decimals(ctx)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Symbol: symbol, Decimals: decimals}, nil
}

// Symbol 调用合约 symbol() 并严格解析字符串返回值。
func (c *Contract) Symbol(ctx context.Context) (string, error) {
	result, err := c.call(ctx, "symbol")
	if err != nil {
		return "", err
	}
	values, err := c.abi.Unpack("symbol", result)
	if err != nil || len(values) != 1 {
		return "", errors.New("解析 ERC-20 symbol 返回值失败")
	}
	symbol, ok := values[0].(string)
	symbol = strings.TrimSpace(symbol)
	if !ok || symbol == "" {
		return "", errors.New("ERC-20 symbol 返回值无效")
	}
	return symbol, nil
}

// Decimals 调用合约 decimals() 并严格解析 uint8 返回值。
func (c *Contract) Decimals(ctx context.Context) (uint8, error) {
	result, err := c.call(ctx, "decimals")
	if err != nil {
		return 0, err
	}
	values, err := c.abi.Unpack("decimals", result)
	if err != nil || len(values) != 1 {
		return 0, errors.New("解析 ERC-20 decimals 返回值失败")
	}
	decimals, ok := values[0].(uint8)
	if !ok {
		return 0, errors.New("ERC-20 decimals 返回值无效")
	}
	return decimals, nil
}

// BalanceOf 调用 balanceOf(address) 查询 Token 最小单位余额。
func (c *Contract) BalanceOf(ctx context.Context, owner common.Address) (*big.Int, error) {
	result, err := c.call(ctx, "balanceOf", owner)
	if err != nil {
		return nil, err
	}
	values, err := c.abi.Unpack("balanceOf", result)
	if err != nil || len(values) != 1 {
		return nil, errors.New("解析 ERC-20 balanceOf 返回值失败")
	}
	balance, ok := values[0].(*big.Int)
	if !ok || balance == nil || balance.Sign() < 0 {
		return nil, errors.New("ERC-20 balanceOf 返回值无效")
	}
	return new(big.Int).Set(balance), nil
}

// EncodeTransfer 使用标准 ABI 编码 transfer(address,uint256) calldata。
func (c *Contract) EncodeTransfer(to common.Address, amountUnits *big.Int) ([]byte, error) {
	if to == (common.Address{}) || amountUnits == nil || amountUnits.Sign() <= 0 {
		return nil, errors.New("ERC-20 transfer 目标地址和金额必须有效")
	}
	calldata, err := c.abi.Pack("transfer", to, amountUnits)
	if err != nil {
		return nil, fmt.Errorf("编码 ERC-20 transfer 失败：%w", err)
	}
	return calldata, nil
}

// DecodeTransferCalldata 严格解码标准 transfer calldata 并返回目标地址和最小单位金额。
func (c *Contract) DecodeTransferCalldata(calldata []byte) (common.Address, *big.Int, error) {
	method, ok := c.abi.Methods["transfer"]
	if !ok || len(calldata) < 4 || !bytes.Equal(calldata[:4], method.ID) {
		return common.Address{}, nil, errors.New("ERC-20 transfer calldata 方法标识无效")
	}
	values, err := method.Inputs.Unpack(calldata[4:])
	if err != nil || len(values) != 2 {
		return common.Address{}, nil, errors.New("解析 ERC-20 transfer calldata 失败")
	}
	to, addressOK := values[0].(common.Address)
	amountUnits, amountOK := values[1].(*big.Int)
	if !addressOK || to == (common.Address{}) || !amountOK || amountUnits == nil || amountUnits.Sign() <= 0 {
		return common.Address{}, nil, errors.New("ERC-20 transfer calldata 参数无效")
	}
	return to, new(big.Int).Set(amountUnits), nil
}

// ValidateTransferResult 校验标准 transfer 调用返回单个 true。
func (c *Contract) ValidateTransferResult(result []byte) error {
	values, err := c.abi.Unpack("transfer", result)
	if err != nil || len(values) != 1 {
		return errors.New("解析 ERC-20 transfer 返回值失败")
	}
	success, ok := values[0].(bool)
	if !ok {
		return errors.New("ERC-20 transfer 返回值类型无效")
	}
	if !success {
		return ErrTransferRejected
	}
	return nil
}

// EstimateTransferGas 估算从指定地址发起 Token transfer 所需 Gas。
func (c *Contract) EstimateTransferGas(ctx context.Context, from, to common.Address, amountUnits *big.Int) (uint64, error) {
	if from == (common.Address{}) {
		return 0, errors.New("估算 ERC-20 transfer Gas 必须提供发送地址")
	}
	calldata, err := c.EncodeTransfer(to, amountUnits)
	if err != nil {
		return 0, err
	}
	gas, err := c.chain.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &c.address, Data: calldata})
	if err != nil {
		return 0, fmt.Errorf("估算 ERC-20 transfer Gas 失败：%w", err)
	}
	if gas == 0 {
		return 0, errors.New("ERC-20 transfer Gas 估算结果无效")
	}
	return gas, nil
}

// FilterTransferLogs 查询指定闭区间内由当前合约产生的 Transfer Event 日志。
func (c *Contract) FilterTransferLogs(ctx context.Context, fromBlock, toBlock uint64) ([]types.Log, error) {
	if fromBlock > toBlock {
		return nil, errors.New("ERC-20 日志查询起始区块不能大于结束区块")
	}
	logs, err := c.chain.FilterLogs(ctx, ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(fromBlock),
		ToBlock:   new(big.Int).SetUint64(toBlock),
		Addresses: []common.Address{c.address},
		Topics:    [][]common.Hash{{c.abi.Events["Transfer"].ID}},
	})
	if err != nil {
		return nil, fmt.Errorf("查询 ERC-20 Transfer Event 失败：%w", err)
	}
	return logs, nil
}

// DecodeTransferLog 严格解码并校验单条标准 Transfer Event。
func (c *Contract) DecodeTransferLog(log types.Log) (TransferEvent, error) {
	if log.Removed {
		return TransferEvent{}, ErrRemovedLog
	}
	transferABI, ok := c.abi.Events["Transfer"]
	if !ok || log.Address != c.address || len(log.Topics) != 3 || log.Topics[0] != transferABI.ID || len(log.Data) != 32 {
		return TransferEvent{}, ErrInvalidTransferLog
	}
	from, err := addressFromTopic(log.Topics[1])
	if err != nil {
		return TransferEvent{}, err
	}
	to, err := addressFromTopic(log.Topics[2])
	if err != nil {
		return TransferEvent{}, err
	}
	values, err := transferABI.Inputs.NonIndexed().Unpack(log.Data)
	if err != nil || len(values) != 1 {
		return TransferEvent{}, ErrInvalidTransferLog
	}
	amountUnits, ok := values[0].(*big.Int)
	if !ok || amountUnits == nil || amountUnits.Sign() <= 0 || log.TxHash == (common.Hash{}) || log.BlockHash == (common.Hash{}) {
		return TransferEvent{}, ErrInvalidTransferLog
	}
	return TransferEvent{
		Contract: c.address, From: from, To: to, AmountUnits: new(big.Int).Set(amountUnits),
		TxHash: log.TxHash, LogIndex: log.Index, BlockNumber: log.BlockNumber, BlockHash: log.BlockHash,
	}, nil
}

// call 编码方法参数、执行只读合约调用并返回原始结果。
func (c *Contract) call(ctx context.Context, method string, arguments ...any) ([]byte, error) {
	calldata, err := c.abi.Pack(method, arguments...)
	if err != nil {
		return nil, fmt.Errorf("编码 ERC-20 %s 调用失败：%w", method, err)
	}
	result, err := c.chain.CallContract(ctx, ethereum.CallMsg{To: &c.address, Data: calldata}, nil)
	if err != nil {
		return nil, fmt.Errorf("调用 ERC-20 %s 失败：%w", method, err)
	}
	return result, nil
}

// addressFromTopic 从严格左侧补零的 indexed address Topic 中提取地址。
func addressFromTopic(topic common.Hash) (common.Address, error) {
	bytes := topic.Bytes()
	for _, value := range bytes[:12] {
		if value != 0 {
			return common.Address{}, ErrInvalidTransferLog
		}
	}
	return common.BytesToAddress(bytes[12:]), nil
}
