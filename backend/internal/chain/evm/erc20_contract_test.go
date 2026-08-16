package evm

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/xiaoqi/mini-custody/backend/internal/token/erc20"
)

// TestERC20ContractRejectsInvalidABIAndRPCRevert 验证真实 JSON-RPC 调用链不会吞掉 ABI 错误或合约回退。
func TestERC20ContractRejectsInvalidABIAndRPCRevert(t *testing.T) {
	mock, _, contract := newRPCContract(t)
	mock.setResult("eth_call", "0x01")
	if _, err := contract.Symbol(context.Background()); err == nil {
		t.Fatal("Symbol() error = nil, want invalid ABI")
	}
	mock.failRPC("eth_call", 3, "execution reverted", -1)
	if _, err := contract.Decimals(context.Background()); err == nil {
		t.Fatal("Decimals() error = nil, want RPC revert")
	}
}

// TestERC20ContractRejectsFalseTransferFromMockRPC 验证 JSON-RPC 返回 transfer=false 时适配器明确拒绝。
func TestERC20ContractRejectsFalseTransferFromMockRPC(t *testing.T) {
	mock, client, contract := newRPCContract(t)
	contractABI, err := erc20.StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	encoded, err := contractABI.Methods["transfer"].Outputs.Pack(false)
	if err != nil {
		t.Fatalf("pack false transfer error = %v", err)
	}
	mock.setResult("eth_call", "0x"+hex.EncodeToString(encoded))
	target := common.HexToAddress("0x2222222222222222222222222222222222222222")
	calldata, err := contract.EncodeTransfer(target, big.NewInt(1_000_000))
	if err != nil {
		t.Fatalf("EncodeTransfer() error = %v", err)
	}
	address := contract.Address()
	result, err := client.CallContract(context.Background(), ethereum.CallMsg{To: &address, Data: calldata}, nil)
	if err != nil {
		t.Fatalf("CallContract() error = %v", err)
	}
	if err := contract.ValidateTransferResult(result); !errors.Is(err, erc20.ErrTransferRejected) {
		t.Fatalf("ValidateTransferResult() error = %v", err)
	}
}

// TestERC20ContractRejectsRemovedLogFromMockRPC 验证 JSON-RPC 返回的 Removed Log 不会进入充值处理。
func TestERC20ContractRejectsRemovedLogFromMockRPC(t *testing.T) {
	mock, _, contract := newRPCContract(t)
	contractABI, err := erc20.StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	data, err := contractABI.Events["Transfer"].Inputs.NonIndexed().Pack(big.NewInt(1_000_000))
	if err != nil {
		t.Fatalf("pack Transfer Event error = %v", err)
	}
	mock.setResult("eth_getLogs", []types.Log{{
		Address: contract.Address(),
		Topics: []common.Hash{
			contractABI.Events["Transfer"].ID,
			common.BytesToHash(common.LeftPadBytes(from.Bytes(), 32)),
			common.BytesToHash(common.LeftPadBytes(to.Bytes(), 32)),
		},
		Data: data, BlockNumber: 100, TxHash: common.HexToHash("0x01"),
		BlockHash: common.HexToHash("0x02"), Index: 1, Removed: true,
	}})
	logs, err := contract.FilterTransferLogs(context.Background(), 100, 100)
	if err != nil || len(logs) != 1 {
		t.Fatalf("FilterTransferLogs() count = %d, error = %v", len(logs), err)
	}
	if _, err := contract.DecodeTransferLog(logs[0]); !errors.Is(err, erc20.ErrRemovedLog) {
		t.Fatalf("DecodeTransferLog() error = %v", err)
	}
}

// newRPCContract 创建经过 Sepolia Chain ID 校验的 Mock JSON-RPC 合约适配器。
func newRPCContract(t *testing.T) (*mockRPC, *Client, *erc20.Contract) {
	t.Helper()
	mock := newMockRPC(t)
	client := newTestClient(t, mock.URL())
	t.Cleanup(client.Close)
	contract, err := erc20.NewContract(client, common.HexToAddress("0x1c7d4b196cb0c7b01d743fbc6116a902379c7238"))
	if err != nil {
		t.Fatalf("NewContract() error = %v", err)
	}
	return mock, client, contract
}
