package wallet

import (
	"context"
	"errors"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	hdwallet "github.com/miguelmota/go-ethereum-hdwallet"
	"github.com/tyler-smith/go-bip39"
)

const (
	EthereumExternalPath = "m/44'/60'/0'/0"
	TreasuryPath         = EthereumExternalPath + "/0"
)

type KeyProvider interface {
	// Address 按派生路径返回托管地址。
	Address(ctx context.Context, path string) (common.Address, error)
	// SignTx 按派生路径签署指定链的交易。
	SignTx(ctx context.Context, path string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}

type MnemonicKeyProvider struct {
	wallet *hdwallet.Wallet
}

// NewMnemonicKeyProvider 使用 BIP-39 助记词创建密钥提供器。
func NewMnemonicKeyProvider(mnemonic string) (*MnemonicKeyProvider, error) {
	mnemonic = strings.TrimSpace(mnemonic)
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, errors.New("BIP-39 助记词无效")
	}
	hdWallet, err := hdwallet.NewFromMnemonic(mnemonic)
	if err != nil {
		return nil, errors.New("初始化 HD 钱包失败")
	}
	return &MnemonicKeyProvider{wallet: hdWallet}, nil
}

// UserPath 根据用户索引生成 Ethereum BIP-44 派生路径。
func UserPath(index uint32) string {
	return EthereumExternalPath + "/" + new(big.Int).SetUint64(uint64(index)).String()
}

// Address 按派生路径计算托管地址。
func (p *MnemonicKeyProvider) Address(_ context.Context, path string) (common.Address, error) {
	derivationPath, err := hdwallet.ParseDerivationPath(path)
	if err != nil {
		return common.Address{}, errors.New("密钥派生路径无效")
	}
	account, err := p.wallet.Derive(derivationPath, false)
	if err != nil {
		return common.Address{}, errors.New("派生钱包地址失败")
	}
	return account.Address, nil
}

// SignTx 使用指定派生路径对应的私钥签署交易。
func (p *MnemonicKeyProvider) SignTx(_ context.Context, path string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	if tx == nil {
		return nil, errors.New("必须提供待签名交易")
	}
	if chainID == nil || chainID.Sign() <= 0 {
		return nil, errors.New("Chain ID 必须大于零")
	}
	derivationPath, err := hdwallet.ParseDerivationPath(path)
	if err != nil {
		return nil, errors.New("密钥派生路径无效")
	}
	account, err := p.wallet.Derive(derivationPath, true)
	if err != nil {
		return nil, errors.New("派生签名账户失败")
	}
	signed, err := p.wallet.SignTx(account, tx, chainID)
	if err != nil {
		return nil, errors.New("签署交易失败")
	}
	return signed, nil
}
