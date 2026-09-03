package btc

import (
	"context"
	"errors"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"strings"
)

type WithdrawalStore interface {
	CreateBTCWithdrawal(context.Context, int64, string, string, int64, int64) (Withdrawal, bool, error)
}
type WithdrawalService struct {
	store   WithdrawalStore
	feeRate int64
}

// NewWithdrawalService 创建 BTC 提币请求服务。
func NewWithdrawalService(store WithdrawalStore, feeRate int64) (*WithdrawalService, error) {
	if store == nil || feeRate <= 0 {
		return nil, errors.New("BTC 提币服务配置无效")
	}
	return &WithdrawalService{store: store, feeRate: feeRate}, nil
}

// Create 校验 Signet 地址和金额并幂等占用用户余额。
func (s *WithdrawalService) Create(ctx context.Context, userID int64, key, address string, amount int64) (Withdrawal, bool, error) {
	key = strings.TrimSpace(key)
	if userID <= 0 || key == "" || amount <= 0 {
		return Withdrawal{}, false, errors.New("BTC 提币请求无效")
	}
	decoded, err := btcutil.DecodeAddress(strings.TrimSpace(address), &chaincfg.SigNetParams)
	if err != nil || !decoded.IsForNet(&chaincfg.SigNetParams) {
		return Withdrawal{}, false, errors.New("BTC 提币目标必须是 Signet 地址")
	}
	return s.store.CreateBTCWithdrawal(ctx, userID, key, decoded.EncodeAddress(), amount, s.feeRate)
}
