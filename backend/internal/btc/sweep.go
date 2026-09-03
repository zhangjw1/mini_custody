package btc

import (
	"bytes"
	"errors"
	"fmt"
	"math"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

const p2wpkhSweepVBytes int64 = 110

// BuildSweep creates and signs a one-input, one-output P2WPKH sweep.
// BuildSweep 构建并签署单输入单输出 Bitcoin 归集交易。
func BuildSweep(provider *wallet.MnemonicKeyProvider, path string, utxo UTXO, destination Address, feeRateSatVB int64) (Sweep, error) {
	if provider == nil || utxo.ValueSats <= 0 || len(utxo.ScriptPubKey) == 0 || len(destination.ScriptPubKey) == 0 || feeRateSatVB <= 0 {
		return Sweep{}, errors.New("BTC 归集参数无效")
	}
	if feeRateSatVB > math.MaxInt64/p2wpkhSweepVBytes {
		return Sweep{}, errors.New("BTC 归集费率超出范围")
	}
	fee := feeRateSatVB * p2wpkhSweepVBytes
	if utxo.ValueSats <= fee {
		return Sweep{}, ErrInsufficientSweepValue
	}
	output := utxo.ValueSats - fee
	if output < DustThresholdSats {
		return Sweep{}, ErrSweepDust
	}
	hash, err := chainhash.NewHashFromStr(utxo.TxID)
	if err != nil {
		return Sweep{}, errors.New("UTXO txid 无效")
	}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(hash, utxo.Vout), nil, nil))
	tx.AddTxOut(wire.NewTxOut(output, destination.ScriptPubKey))
	raw, err := provider.SignBitcoinInput(nil, path, tx, 0, utxo.ValueSats, utxo.ScriptPubKey)
	if err != nil {
		return Sweep{}, err
	}
	var encoded bytes.Buffer
	encoded.Write(raw)
	txHash := tx.TxHash()
	return Sweep{UTXO: utxo, To: destination, InputValueSats: utxo.ValueSats, OutputValueSats: output, FeeSats: fee, FeeRateSatVB: feeRateSatVB, RawTx: encoded.Bytes(), TxID: txHash.String(), Status: SweepSigned}, nil
}

// ValidateSweep 校验归集交易金额守恒和签名材料完整性。
func ValidateSweep(s Sweep) error {
	if s.InputValueSats <= 0 || s.OutputValueSats <= 0 || s.FeeSats <= 0 || s.InputValueSats != s.OutputValueSats+s.FeeSats {
		return fmt.Errorf("BTC 归集金额守恒校验失败")
	}
	if len(s.RawTx) == 0 || s.Status == "" {
		return errors.New("BTC 归集缺少已签名交易")
	}
	return nil
}
