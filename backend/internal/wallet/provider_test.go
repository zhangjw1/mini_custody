package wallet

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const testMnemonic = "tag volcano eight thank tide danger coast health above argue embrace heavy"

// TestMnemonicKeyProviderDerivesPublishedVector 验证公开 BIP-44 测试向量。
func TestMnemonicKeyProviderDerivesPublishedVector(t *testing.T) {
	provider, err := NewMnemonicKeyProvider(testMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}

	tests := []struct {
		path string
		want string
	}{
		{path: "m/44'/60'/0'/0/0", want: "0xC49926C4124cEe1cbA0Ea94Ea31a6c12318df947"},
		{path: "m/44'/60'/0'/0/1", want: "0x8230645ac28a4edd1b0b53e7cd8019744e9dd559"},
	}

	for _, tt := range tests {
		got, err := provider.Address(context.Background(), tt.path)
		if err != nil {
			t.Fatalf("Address(%q) error = %v", tt.path, err)
		}
		if got != common.HexToAddress(tt.want) {
			t.Fatalf("Address(%q) = %s, want %s", tt.path, got.Hex(), tt.want)
		}
	}
}

// TestMnemonicKeyProviderSignsDynamicFeeTransaction 验证 EIP-1559 交易签名和发送方恢复。
func TestMnemonicKeyProviderSignsDynamicFeeTransaction(t *testing.T) {
	provider, err := NewMnemonicKeyProvider(testMnemonic)
	if err != nil {
		t.Fatalf("NewMnemonicKeyProvider() error = %v", err)
	}
	chainID := big.NewInt(11155111)
	to := common.HexToAddress("0x1111111111111111111111111111111111111111")
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     7,
		GasTipCap: big.NewInt(1_000_000_000),
		GasFeeCap: big.NewInt(2_000_000_000),
		Gas:       21_000,
		To:        &to,
		Value:     big.NewInt(10_000_000_000_000_000),
	})

	signed, err := provider.SignTx(context.Background(), TreasuryPath, tx, chainID)
	if err != nil {
		t.Fatalf("SignTx() error = %v", err)
	}
	if signed.Type() != types.DynamicFeeTxType {
		t.Fatalf("signed transaction type = %d, want %d", signed.Type(), types.DynamicFeeTxType)
	}
	sender, err := types.Sender(types.LatestSignerForChainID(chainID), signed)
	if err != nil {
		t.Fatalf("recover sender: %v", err)
	}
	want, err := provider.Address(context.Background(), TreasuryPath)
	if err != nil {
		t.Fatalf("derive expected sender: %v", err)
	}
	if sender != want {
		t.Fatalf("sender = %s, want %s", sender.Hex(), want.Hex())
	}
}

// TestEncryptedRootRoundTripAndFilePermissions 验证加密根密钥读写和文件权限。
func TestEncryptedRootRoundTripAndFilePermissions(t *testing.T) {
	const password = "correct horse battery staple"
	encrypted, err := EncryptMnemonic(testMnemonic, password)
	if err != nil {
		t.Fatalf("EncryptMnemonic() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "nested", "custody-root.age")
	if err := WriteEncryptedRoot(path, encrypted); err != nil {
		t.Fatalf("WriteEncryptedRoot() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file permissions = %o, want 600", got)
	}
	provider, err := LoadProvider(path, password)
	if err != nil {
		t.Fatalf("LoadProvider() error = %v", err)
	}
	address, err := provider.Address(context.Background(), TreasuryPath)
	if err != nil {
		t.Fatalf("Address() error = %v", err)
	}
	if want := common.HexToAddress("0xC49926C4124cEe1cbA0Ea94Ea31a6c12318df947"); address != want {
		t.Fatalf("address = %s, want %s", address.Hex(), want.Hex())
	}
}

// TestDecryptMnemonicDoesNotLeakSecretsOnFailure 验证解密失败错误不会泄露敏感信息。
func TestDecryptMnemonicDoesNotLeakSecretsOnFailure(t *testing.T) {
	const (
		password      = "right-password"
		wrongPassword = "wrong-password"
	)
	encrypted, err := EncryptMnemonic(testMnemonic, password)
	if err != nil {
		t.Fatalf("EncryptMnemonic() error = %v", err)
	}
	_, err = DecryptMnemonic(encrypted, wrongPassword)
	if err == nil {
		t.Fatal("DecryptMnemonic() error = nil, want failure")
	}
	errorText := err.Error()
	for _, secret := range []string{password, wrongPassword, testMnemonic} {
		if strings.Contains(errorText, secret) {
			t.Fatalf("error contains secret %q", secret)
		}
	}
}
