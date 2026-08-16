package erc20

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestStandardABIMethodIDs 验证标准 ERC-20 方法选择器没有被 ABI 修改破坏。
func TestStandardABIMethodIDs(t *testing.T) {
	contractABI, err := StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	expected := map[string]string{
		"symbol":    "95d89b41",
		"decimals":  "313ce567",
		"balanceOf": "70a08231",
		"transfer":  "a9059cbb",
	}
	for methodName, expectedID := range expected {
		method, ok := contractABI.Methods[methodName]
		if !ok {
			t.Fatalf("method %s is missing", methodName)
		}
		if actualID := hex.EncodeToString(method.ID); actualID != expectedID {
			t.Fatalf("method %s ID = %s, want %s", methodName, actualID, expectedID)
		}
	}
}

// TestStandardABITransferTopic 验证 Transfer Event 签名和索引参数布局。
func TestStandardABITransferTopic(t *testing.T) {
	contractABI, err := StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	transferEvent, ok := contractABI.Events["Transfer"]
	if !ok {
		t.Fatal("Transfer event is missing")
	}
	wantTopic := common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	if transferEvent.ID != wantTopic {
		t.Fatalf("Transfer topic = %s, want %s", transferEvent.ID.Hex(), wantTopic.Hex())
	}
	if len(transferEvent.Inputs) != 3 || !transferEvent.Inputs[0].Indexed || !transferEvent.Inputs[1].Indexed || transferEvent.Inputs[2].Indexed {
		t.Fatalf("Transfer inputs = %+v", transferEvent.Inputs)
	}
}

// TestStandardABIPacksTransfer 验证 transfer calldata 由结构化 ABI 正确编码。
func TestStandardABIPacksTransfer(t *testing.T) {
	contractABI, err := StandardABI()
	if err != nil {
		t.Fatalf("StandardABI() error = %v", err)
	}
	target := common.HexToAddress("0x1111111111111111111111111111111111111111")
	calldata, err := contractABI.Pack("transfer", target, big.NewInt(1234567))
	if err != nil {
		t.Fatalf("Pack() error = %v", err)
	}
	if len(calldata) != 68 || hex.EncodeToString(calldata[:4]) != "a9059cbb" {
		t.Fatalf("transfer calldata = %x", calldata)
	}
}
