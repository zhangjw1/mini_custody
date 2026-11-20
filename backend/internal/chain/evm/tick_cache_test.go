package evm

import (
	"context"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/core/types"
)

// TestTickCacheDeduplicatesWithinSingleTick 验证单轮 TickCache 复用区块高度、区块头、优先费及泛型值，而在无缓存上下文时直连查询。
func TestTickCacheDeduplicatesWithinSingleTick(t *testing.T) {
	var blockCalls, headerCalls, tipCalls, walletCalls int
	fetchBlock := func(context.Context) (uint64, error) {
		blockCalls++
		return 100, nil
	}
	fetchHeader := func(context.Context, *big.Int) (*types.Header, error) {
		headerCalls++
		return &types.Header{Number: big.NewInt(100), BaseFee: big.NewInt(10)}, nil
	}
	fetchTip := func(context.Context) (*big.Int, error) {
		tipCalls++
		return big.NewInt(2), nil
	}
	fetchWallet := func(context.Context) (string, error) {
		walletCalls++
		return "0xhot", nil
	}

	ctx := WithTickCache(context.Background())
	for i := 0; i < 5; i++ {
		if b, err := CachedBlockNumber(ctx, fetchBlock); err != nil || b != 100 {
			t.Fatalf("CachedBlockNumber() = %d, err = %v", b, err)
		}
		if h, err := CachedLatestHeader(ctx, fetchHeader); err != nil || h.BaseFee.Int64() != 10 {
			t.Fatalf("CachedLatestHeader() = %+v, err = %v", h, err)
		}
		tip, err := CachedGasTipCap(ctx, fetchTip)
		if err != nil || tip.Int64() != 2 {
			t.Fatalf("CachedGasTipCap() = %v, err = %v", tip, err)
		}
		tip.SetInt64(999) // 验证返回副本不会污染缓存值
		if w, err := CachedValue(ctx, "hot", fetchWallet); err != nil || w != "0xhot" {
			t.Fatalf("CachedValue() = %q, err = %v", w, err)
		}
	}
	if blockCalls != 1 || headerCalls != 1 || tipCalls != 1 || walletCalls != 1 {
		t.Fatalf("expected 1 call each inside TickCache, got block=%d header=%d tip=%d wallet=%d",
			blockCalls, headerCalls, tipCalls, walletCalls)
	}

	// 验证未挂载 TickCache 的独立调用仍会直接发起查询。
	if _, err := CachedBlockNumber(context.Background(), fetchBlock); err != nil {
		t.Fatalf("uncached CachedBlockNumber() error = %v", err)
	}
	if blockCalls != 2 {
		t.Fatalf("expected uncached call to increment blockCalls to 2, got %d", blockCalls)
	}
}
