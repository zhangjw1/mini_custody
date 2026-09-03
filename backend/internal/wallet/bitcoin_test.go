package wallet

import (
	"bytes"
	"context"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// TestBitcoinAddressDerivesStableSignetP2WPKH 验证地址派生稳定性。
func TestBitcoinAddressDerivesStableSignetP2WPKH(t *testing.T) {
	provider, err := NewMnemonicKeyProvider(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	first, err := provider.BitcoinAddress(context.Background(), BitcoinUserPath(1))
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.BitcoinAddress(context.Background(), BitcoinUserPath(1))
	if err != nil {
		t.Fatal(err)
	}
	if first.Address != second.Address || len(first.ScriptPubKey) != 22 || !bytes.Equal(first.ScriptPubKey, second.ScriptPubKey) {
		t.Fatalf("unstable P2WPKH derivation: %#v %#v", first, second)
	}
	if first.Address[:3] != "tb1" {
		t.Fatalf("address = %s, want Signet tb1", first.Address)
	}
}

// TestSignBitcoinInputProducesValidWitness 验证签名见证数据可执行。
func TestSignBitcoinInputProducesValidWitness(t *testing.T) {
	provider, err := NewMnemonicKeyProvider(testMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	source, err := provider.BitcoinAddress(context.Background(), BitcoinUserPath(1))
	if err != nil {
		t.Fatal(err)
	}
	target, err := provider.BitcoinAddress(context.Background(), BitcoinTreasuryPath)
	if err != nil {
		t.Fatal(err)
	}
	prevHash := chainhash.DoubleHashH([]byte("controlled-utxo"))
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&prevHash, 0), nil, nil))
	tx.AddTxOut(wire.NewTxOut(99_000, target.ScriptPubKey))
	raw, err := provider.SignBitcoinInput(context.Background(), BitcoinUserPath(1), tx, 0, 100_000, source.ScriptPubKey)
	if err != nil {
		t.Fatal(err)
	}
	decoded := wire.NewMsgTx(2)
	if err := decoded.Deserialize(bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(source.ScriptPubKey, 100_000)
	engine, err := txscript.NewEngine(source.ScriptPubKey, decoded, 0, txscript.StandardVerifyFlags, nil, txscript.NewTxSigHashes(decoded, fetcher), 100_000, fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Execute(); err != nil {
		t.Fatalf("signed witness rejected: %v", err)
	}
}
