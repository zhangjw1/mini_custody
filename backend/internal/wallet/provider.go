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
	Address(ctx context.Context, path string) (common.Address, error)
	SignTx(ctx context.Context, path string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}

type MnemonicKeyProvider struct {
	wallet *hdwallet.Wallet
}

func NewMnemonicKeyProvider(mnemonic string) (*MnemonicKeyProvider, error) {
	mnemonic = strings.TrimSpace(mnemonic)
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, errors.New("invalid BIP-39 mnemonic")
	}
	hdWallet, err := hdwallet.NewFromMnemonic(mnemonic)
	if err != nil {
		return nil, errors.New("initialize HD wallet")
	}
	return &MnemonicKeyProvider{wallet: hdWallet}, nil
}

func UserPath(index uint32) string {
	return EthereumExternalPath + "/" + new(big.Int).SetUint64(uint64(index)).String()
}

func (p *MnemonicKeyProvider) Address(_ context.Context, path string) (common.Address, error) {
	derivationPath, err := hdwallet.ParseDerivationPath(path)
	if err != nil {
		return common.Address{}, errors.New("invalid derivation path")
	}
	account, err := p.wallet.Derive(derivationPath, false)
	if err != nil {
		return common.Address{}, errors.New("derive wallet address")
	}
	return account.Address, nil
}

func (p *MnemonicKeyProvider) SignTx(_ context.Context, path string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error) {
	if tx == nil {
		return nil, errors.New("transaction is required")
	}
	if chainID == nil || chainID.Sign() <= 0 {
		return nil, errors.New("positive chain ID is required")
	}
	derivationPath, err := hdwallet.ParseDerivationPath(path)
	if err != nil {
		return nil, errors.New("invalid derivation path")
	}
	account, err := p.wallet.Derive(derivationPath, true)
	if err != nil {
		return nil, errors.New("derive signing account")
	}
	signed, err := p.wallet.SignTx(account, tx, chainID)
	if err != nil {
		return nil, errors.New("sign transaction")
	}
	return signed, nil
}
