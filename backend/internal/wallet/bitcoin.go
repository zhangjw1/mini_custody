package wallet

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

const (
	BitcoinExternalPath = "m/84'/1'/0'/0"
	BitcoinChangePath   = "m/84'/1'/0'/1"
	BitcoinTreasuryPath = BitcoinChangePath + "/0"
)

// BitcoinAddress describes a controlled Signet P2WPKH output.
type BitcoinAddress struct {
	Address      string
	ScriptPubKey []byte
}

// BitcoinUserPath returns the BIP-84 Signet receive path for a user index.
// BitcoinUserPath 返回用户 Signet 接收地址派生路径。
func BitcoinUserPath(index uint32) string {
	return fmt.Sprintf("%s/%d", BitcoinExternalPath, index)
}

// BitcoinAddress derives a native SegWit address and its output script.
// BitcoinAddress 派生 Signet P2WPKH 地址及输出脚本。
func (p *MnemonicKeyProvider) BitcoinAddress(_ context.Context, path string) (BitcoinAddress, error) {
	key, err := p.bitcoinKey(path)
	if err != nil {
		return BitcoinAddress{}, err
	}
	pub, err := key.ECPubKey()
	if err != nil {
		return BitcoinAddress{}, errors.New("派生 Bitcoin 公钥失败")
	}
	address, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub.SerializeCompressed()), &chaincfg.SigNetParams)
	if err != nil {
		return BitcoinAddress{}, errors.New("创建 Signet P2WPKH 地址失败")
	}
	script, err := txscript.PayToAddrScript(address)
	if err != nil {
		return BitcoinAddress{}, errors.New("创建 Signet P2WPKH 脚本失败")
	}
	return BitcoinAddress{Address: address.EncodeAddress(), ScriptPubKey: script}, nil
}

// SignBitcoinInput signs one controlled P2WPKH input and returns serialized transaction bytes.
// SignBitcoinInput 签署一个受控 P2WPKH 输入并返回原始交易。
func (p *MnemonicKeyProvider) SignBitcoinInput(_ context.Context, path string, tx *wire.MsgTx, inputIndex int, amountSats int64, scriptPubKey []byte) ([]byte, error) {
	if tx == nil || inputIndex < 0 || inputIndex >= len(tx.TxIn) || amountSats <= 0 || len(scriptPubKey) == 0 {
		return nil, errors.New("Bitcoin 签名参数无效")
	}
	key, err := p.bitcoinKey(path)
	if err != nil {
		return nil, err
	}
	privateKey, err := key.ECPrivKey()
	if err != nil {
		return nil, errors.New("派生 Bitcoin 签名私钥失败")
	}
	fetcher := txscript.NewCannedPrevOutputFetcher(scriptPubKey, amountSats)
	hashes := txscript.NewTxSigHashes(tx, fetcher)
	witness, err := txscript.WitnessSignature(tx, hashes, inputIndex, amountSats, scriptPubKey, txscript.SigHashAll, privateKey, true)
	if err != nil {
		return nil, errors.New("签署 Bitcoin P2WPKH 输入失败")
	}
	tx.TxIn[inputIndex].Witness = witness
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		return nil, errors.New("编码已签名 Bitcoin 交易失败")
	}
	return encoded.Bytes(), nil
}

// bitcoinKey 按严格 BIP-84 Signet 路径派生扩展密钥。
func (p *MnemonicKeyProvider) bitcoinKey(path string) (*hdkeychain.ExtendedKey, error) {
	indices, err := parseBitcoinPath(path)
	if err != nil {
		return nil, err
	}
	key, err := hdkeychain.NewMaster(p.seed, &chaincfg.SigNetParams)
	if err != nil {
		return nil, errors.New("创建 Bitcoin HD 根密钥失败")
	}
	for _, index := range indices {
		key, err = key.Derive(index)
		if err != nil {
			return nil, errors.New("派生 Bitcoin 子密钥失败")
		}
	}
	return key, nil
}

// parseBitcoinPath 解析并校验 BIP-84 Signet 派生路径。
func parseBitcoinPath(path string) ([]uint32, error) {
	const hardened = uint32(hdkeychain.HardenedKeyStart)
	var purpose, coin, account, branch, index uint32
	if _, err := fmt.Sscanf(path, "m/%d'/%d'/%d'/%d/%d", &purpose, &coin, &account, &branch, &index); err != nil || purpose != 84 || coin != 1 || account != 0 || branch > 1 {
		return nil, errors.New("Bitcoin 派生路径无效")
	}
	return []uint32{purpose + hardened, coin + hardened, account + hardened, branch, index}, nil
}
