package btc

import (
	"context"
	"errors"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
)

// WithdrawalStore 定义创建 BTC 提币请求所需的持久化能力。
type WithdrawalStore interface {
	CreateBTCWithdrawal(context.Context, int64, string, string, int64, int64) (Withdrawal, bool, error)
}

// WithdrawalService 负责校验并幂等创建 BTC 提币请求。
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

// Create 校验目标网络地址、dust 下限并幂等占用用户余额。
func (s *WithdrawalService) Create(ctx context.Context, userID int64, key, address string, amount int64) (Withdrawal, bool, error) {
	key = strings.TrimSpace(key)
	if userID <= 0 || key == "" || amount <= 0 {
		return Withdrawal{}, false, errors.New("BTC 提币请求无效")
	}
	if amount < DustThresholdSats {
		return Withdrawal{}, false, errors.New("BTC 提币金额低于 dust 下限")
	}
	params, err := ChainParamsForNetwork(Network)
	if err != nil {
		return Withdrawal{}, false, err
	}
	decoded, err := btcutil.DecodeAddress(strings.TrimSpace(address), params)
	if err != nil || !decoded.IsForNet(params) {
		return Withdrawal{}, false, errors.New("BTC 提币目标地址与当前测试网络不匹配")
	}
	return s.store.CreateBTCWithdrawal(ctx, userID, key, decoded.EncodeAddress(), amount, s.feeRate)
}
