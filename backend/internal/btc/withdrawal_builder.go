package btc

import (
	"bytes"
	"errors"
	"math"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

// BuildWithdrawal 构建并签署多输入 P2WPKH 提币交易，找零发送到平台地址。
func BuildWithdrawal(provider *wallet.MnemonicKeyProvider, inputs []UTXO, inputAddresses []Address, targetScript, changeScript []byte, amountSats, feeRateSatVB int64) ([]byte, int64, int64, error) {
	if provider == nil || len(inputs) == 0 || len(inputs) != len(inputAddresses) || len(targetScript) == 0 || len(changeScript) == 0 || amountSats <= 0 || feeRateSatVB <= 0 {
		return nil, 0, 0, errors.New("BTC 提币构建参数无效")
	}
	if feeRateSatVB > math.MaxInt64/int64(10+68*len(inputs)+31+31) {
		return nil, 0, 0, errors.New("BTC 提币费率超出范围")
	}
	fee := feeRateSatVB * int64(10+68*len(inputs)+31+31)
	total := int64(0)
	tx := wire.NewMsgTx(2)
	for _, item := range inputs {
		hash, err := chainhash.NewHashFromStr(item.TxID)
		if err != nil {
			return nil, 0, 0, errors.New("BTC 提币输入 txid 无效")
		}
		tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(hash, item.Vout), nil, nil))
		if item.ValueSats <= 0 || total > math.MaxInt64-item.ValueSats {
			return nil, 0, 0, errors.New("BTC 提币输入金额无效")
		}
		total += item.ValueSats
	}
	if total < amountSats+fee {
		return nil, 0, 0, errors.New("BTC 提币余额不足")
	}
	change := total - amountSats - fee
	tx.AddTxOut(wire.NewTxOut(amountSats, targetScript))
	if change >= DustThresholdSats {
		tx.AddTxOut(wire.NewTxOut(change, changeScript))
	} else {
		fee += change
		change = 0
	}
	for i, item := range inputs {
		raw, err := provider.SignBitcoinInput(nil, inputAddresses[i].Path, tx, i, item.ValueSats, item.ScriptPubKey)
		if err != nil {
			return nil, 0, 0, err
		}
		tx = wire.NewMsgTx(2)
		if err = tx.Deserialize(bytes.NewReader(raw)); err != nil {
			return nil, 0, 0, err
		}
	}
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		return nil, 0, 0, err
	}
	return encoded.Bytes(), fee, change, nil
}
