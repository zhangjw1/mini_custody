package erc20

import (
	"context"
	"errors"
	"math/big"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type fakeChain struct {
	code       []byte
	callResult map[string][]byte
	callErr    error
	logs       []types.Log
	gas        uint64
	lastCall   ethereum.CallMsg
	lastQuery  ethereum.FilterQuery
}

// CodeAt 返回测试配置的合约代码。
func (f *fakeChain) CodeAt(context.Context, common.Address, *big.Int) ([]byte, error) {
	return f.code, nil
}

// CallContract 根据四字节方法 ID 返回测试编码结果。
func (f *fakeChain) CallContract(_ context.Context, call ethereum.CallMsg, _ *big.Int) ([]byte, error) {
	f.lastCall = call
	if f.callErr != nil {
		return nil, f.callErr
	}
	if len(call.Data) < 4 {
		return nil, nil
	}
	return f.callResult[string(call.Data[:4])], nil
}

// FilterLogs 返回测试配置的 Event 日志。
func (f *fakeChain) FilterLogs(_ context.Context, query ethereum.FilterQuery) ([]types.Log, error) {
	f.lastQuery = query
	return f.logs, nil
}

// EstimateGas 返回测试配置的 Gas 估算值并记录调用参数。
func (f *fakeChain) EstimateGas(_ context.Context, call ethereum.CallMsg) (uint64, error) {
	f.lastCall = call
	return f.gas, nil
}

// TestContractReadsAndValidatesMetadata 验证 symbol、decimals、balanceOf 和启动元数据校验。
func TestContractReadsAndValidatesMetadata(t *testing.T) {
	chain, contract := configuredContract(t)
	metadata, err := contract.Validate(context.Background(), "USDC", 6)
	if err != nil || metadata.Symbol != "USDC" || metadata.Decimals != 6 {
		t.Fatalf("Validate() = %+v, error = %v", metadata, err)
	}
	balance, err := contract.BalanceOf(context.Background(), common.HexToAddress("0x1111111111111111111111111111111111111111"))
	if err != nil || balance.Cmp(big.NewInt(1_234_567)) != 0 {
		t.Fatalf("BalanceOf() = %v, error = %v", balance, err)
	}
	if chain.lastCall.To == nil || *chain.lastCall.To != contract.Address() {
		t.Fatalf("balanceOf target = %v", chain.lastCall.To)
	}
}

// TestContractRejectsMissingCodeAndMetadataMismatch 验证空合约和错误元数据无法通过启动校验。
func TestContractRejectsMissingCodeAndMetadataMismatch(t *testing.T) {
	chain, contract := configuredContract(t)
	chain.code = nil
	if _, err := contract.Validate(context.Background(), "USDC", 6); !errors.Is(err, ErrContractCodeMissing) {
		t.Fatalf("Validate() error = %v, want missing code", err)
	}
	chain.code = []byte{0x60, 0x00}
	if _, err := contract.Validate(context.Background(), "USDT", 6); !errors.Is(err, ErrMetadataMismatch) {
		t.Fatalf("Validate() error = %v, want metadata mismatch", err)
	}
}

// TestContractRejectsInvalidABIAndRevert 验证无效 ABI 返回值和合约回退都不会被解释为零值。
func TestContractRejectsInvalidABIAndRevert(t *testing.T) {
	chain, contract := configuredContract(t)
	chain.callResult[string(contract.abi.Methods["symbol"].ID)] = []byte{0x01}
	if _, err := contract.Symbol(context.Background()); err == nil {
		t.Fatal("Symbol() error = nil, want invalid ABI")
	}
	chain.callErr = errors.New("合约执行回退")
	if _, err := contract.Decimals(context.Background()); err == nil {
		t.Fatal("Decimals() error = nil, want revert")
	}
}

// TestContractEncodesTransferAndValidatesResult 验证 transfer calldata、true 和 false 返回值处理。
func TestContractEncodesTransferAndValidatesResult(t *testing.T) {
	_, contract := configuredContract(t)
	target := common.HexToAddress("0x2222222222222222222222222222222222222222")
	calldata, err := contract.EncodeTransfer(target, big.NewInt(500_000))
	if err != nil || len(calldata) != 68 || string(calldata[:4]) != string(contract.abi.Methods["transfer"].ID) {
		t.Fatalf("EncodeTransfer() = %x, error = %v", calldata, err)
	}
	decodedTarget, decodedAmount, err := contract.DecodeTransferCalldata(calldata)
	if err != nil || decodedTarget != target || decodedAmount.Cmp(big.NewInt(500_000)) != 0 {
		t.Fatalf("DecodeTransferCalldata() target = %s, amount = %v, error = %v", decodedTarget.Hex(), decodedAmount, err)
	}
	if _, _, err := contract.DecodeTransferCalldata([]byte{0x01}); err == nil {
		t.Fatal("DecodeTransferCalldata(invalid) error = nil")
	}
	trueResult, err := contract.abi.Methods["transfer"].Outputs.Pack(true)
	if err != nil {
		t.Fatalf("pack true result error = %v", err)
	}
	if err := contract.ValidateTransferResult(trueResult); err != nil {
		t.Fatalf("ValidateTransferResult(true) error = %v", err)
	}
	falseResult, err := contract.abi.Methods["transfer"].Outputs.Pack(false)
	if err != nil {
		t.Fatalf("pack false result error = %v", err)
	}
	if err := contract.ValidateTransferResult(falseResult); !errors.Is(err, ErrTransferRejected) {
		t.Fatalf("ValidateTransferResult(false) error = %v", err)
	}
	if err := contract.ValidateTransferResult([]byte{0x01}); err == nil {
		t.Fatal("ValidateTransferResult(invalid) error = nil")
	}
}

// TestContractEstimatesTransferGas 验证 Gas 估算使用 Token 合约目标和 ABI calldata。
func TestContractEstimatesTransferGas(t *testing.T) {
	chain, contract := configuredContract(t)
	chain.gas = 65_000
	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	gas, err := contract.EstimateTransferGas(context.Background(), from, to, big.NewInt(1_000_000))
	if err != nil || gas != 65_000 {
		t.Fatalf("EstimateTransferGas() = %d, error = %v", gas, err)
	}
	if chain.lastCall.To == nil || *chain.lastCall.To != contract.Address() || chain.lastCall.From != from || len(chain.lastCall.Data) != 68 {
		t.Fatalf("EstimateGas call = %+v", chain.lastCall)
	}
}

// TestContractFiltersAndDecodesTransferLog 验证日志过滤条件和标准 Transfer Event 严格解码。
func TestContractFiltersAndDecodesTransferLog(t *testing.T) {
	chain, contract := configuredContract(t)
	log := validTransferLog(t, contract)
	chain.logs = []types.Log{log}
	logs, err := contract.FilterTransferLogs(context.Background(), 100, 120)
	if err != nil || len(logs) != 1 {
		t.Fatalf("FilterTransferLogs() count = %d, error = %v", len(logs), err)
	}
	if len(chain.lastQuery.Addresses) != 1 || chain.lastQuery.Addresses[0] != contract.Address() ||
		chain.lastQuery.FromBlock.Uint64() != 100 || chain.lastQuery.ToBlock.Uint64() != 120 {
		t.Fatalf("FilterQuery = %+v", chain.lastQuery)
	}
	event, err := contract.DecodeTransferLog(log)
	if err != nil || event.AmountUnits.Cmp(big.NewInt(1_234_567)) != 0 || event.LogIndex != 3 {
		t.Fatalf("DecodeTransferLog() = %+v, error = %v", event, err)
	}
}

// TestContractRejectsMalformedAndRemovedTransferLogs 验证 Removed、Topic、data 和区块字段异常会被拒绝。
func TestContractRejectsMalformedAndRemovedTransferLogs(t *testing.T) {
	_, contract := configuredContract(t)
	valid := validTransferLog(t, contract)
	testCases := []struct {
		name string
		edit func(*types.Log)
		want error
	}{
		{name: "已移除", edit: func(log *types.Log) { log.Removed = true }, want: ErrRemovedLog},
		{name: "Topic 数量", edit: func(log *types.Log) { log.Topics = log.Topics[:2] }, want: ErrInvalidTransferLog},
		{name: "地址补零", edit: func(log *types.Log) { log.Topics[1][0] = 1 }, want: ErrInvalidTransferLog},
		{name: "数据长度", edit: func(log *types.Log) { log.Data = log.Data[:31] }, want: ErrInvalidTransferLog},
		{name: "交易哈希", edit: func(log *types.Log) { log.TxHash = common.Hash{} }, want: ErrInvalidTransferLog},
		{name: "区块哈希", edit: func(log *types.Log) { log.BlockHash = common.Hash{} }, want: ErrInvalidTransferLog},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			log := valid
			log.Topics = append([]common.Hash(nil), valid.Topics...)
			log.Data = append([]byte(nil), valid.Data...)
			testCase.edit(&log)
			if _, err := contract.DecodeTransferLog(log); !errors.Is(err, testCase.want) {
				t.Fatalf("DecodeTransferLog() error = %v, want %v", err, testCase.want)
			}
		})
	}
}

// configuredContract 创建带标准方法返回值的测试合约适配器。
func configuredContract(t *testing.T) (*fakeChain, *Contract) {
	t.Helper()
	chain := &fakeChain{code: []byte{0x60, 0x00}, callResult: make(map[string][]byte), gas: 65_000}
	contract, err := NewContract(chain, common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"))
	if err != nil {
		t.Fatalf("NewContract() error = %v", err)
	}
	outputs := map[string]any{"symbol": "USDC", "decimals": uint8(6), "balanceOf": big.NewInt(1_234_567)}
	for method, value := range outputs {
		encoded, err := contract.abi.Methods[method].Outputs.Pack(value)
		if err != nil {
			t.Fatalf("pack %s output error = %v", method, err)
		}
		chain.callResult[string(contract.abi.Methods[method].ID)] = encoded
	}
	return chain, contract
}

// validTransferLog 构造包含标准 Topic 和非索引金额的有效 Transfer Event。
func validTransferLog(t *testing.T, contract *Contract) types.Log {
	t.Helper()
	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	data, err := contract.abi.Events["Transfer"].Inputs.NonIndexed().Pack(big.NewInt(1_234_567))
	if err != nil {
		t.Fatalf("pack Transfer data error = %v", err)
	}
	return types.Log{
		Address: contract.Address(),
		Topics: []common.Hash{
			contract.abi.Events["Transfer"].ID,
			common.BytesToHash(common.LeftPadBytes(from.Bytes(), 32)),
			common.BytesToHash(common.LeftPadBytes(to.Bytes(), 32)),
		},
		Data: data, BlockNumber: 100, TxHash: common.HexToHash("0x01"),
		BlockHash: common.HexToHash("0x02"), Index: 3,
	}
}
