package evm

import (
	"context"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/core/types"
)

type tickCacheContextKey struct{}

// TickCache 保存单轮 Worker 轮询内的只读链上与热钱包查询缓存，避免同轮内重复往返。
type TickCache struct {
	mu              sync.Mutex
	hasBlockNumber  bool
	blockNumber     uint64
	hasLatestHeader bool
	latestHeader    *types.Header
	hasTipCap       bool
	tipCap          *big.Int
	values          map[string]any
}

// WithTickCache 为上下文挂载一个新的单轮只读缓存实例。
func WithTickCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, tickCacheContextKey{}, &TickCache{
		values: make(map[string]any),
	})
}

// tickCacheFromContext 从上下文读取当前轮次的只读缓存；未挂载时返回 nil。
func tickCacheFromContext(ctx context.Context) *TickCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(tickCacheContextKey{}).(*TickCache)
	return cache
}

// CachedBlockNumber 在单轮缓存存在时复用最新区块高度查询结果。
func CachedBlockNumber(ctx context.Context, fetch func(context.Context) (uint64, error)) (uint64, error) {
	cache := tickCacheFromContext(ctx)
	if cache == nil {
		return fetch(ctx)
	}
	cache.mu.Lock()
	if cache.hasBlockNumber {
		val := cache.blockNumber
		cache.mu.Unlock()
		return val, nil
	}
	cache.mu.Unlock()

	val, err := fetch(ctx)
	if err != nil {
		return 0, err
	}
	cache.mu.Lock()
	cache.blockNumber = val
	cache.hasBlockNumber = true
	cache.mu.Unlock()
	return val, nil
}

// CachedLatestHeader 在单轮缓存存在时复用 latest 区块头查询结果。
func CachedLatestHeader(ctx context.Context, fetch func(context.Context, *big.Int) (*types.Header, error)) (*types.Header, error) {
	cache := tickCacheFromContext(ctx)
	if cache == nil {
		return fetch(ctx, nil)
	}
	cache.mu.Lock()
	if cache.hasLatestHeader {
		header := cache.latestHeader
		cache.mu.Unlock()
		return header, nil
	}
	cache.mu.Unlock()

	header, err := fetch(ctx, nil)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	if header != nil {
		cache.latestHeader = types.CopyHeader(header)
	}
	cache.hasLatestHeader = true
	cache.mu.Unlock()
	return header, nil
}

// CachedGasTipCap 在单轮缓存存在时复用建议优先费查询结果，并返回独立大整数副本。
func CachedGasTipCap(ctx context.Context, fetch func(context.Context) (*big.Int, error)) (*big.Int, error) {
	cache := tickCacheFromContext(ctx)
	if cache == nil {
		return fetch(ctx)
	}
	cache.mu.Lock()
	if cache.hasTipCap {
		var tip *big.Int
		if cache.tipCap != nil {
			tip = new(big.Int).Set(cache.tipCap)
		}
		cache.mu.Unlock()
		return tip, nil
	}
	cache.mu.Unlock()

	tip, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	cache.mu.Lock()
	if tip != nil {
		cache.tipCap = new(big.Int).Set(tip)
	} else {
		cache.tipCap = nil
	}
	cache.hasTipCap = true
	cache.mu.Unlock()
	if tip == nil {
		return nil, nil
	}
	return new(big.Int).Set(tip), nil
}

// CachedValue 按键在单轮缓存内复用只读查询结果（例如平台热钱包基础信息）。
func CachedValue[T any](ctx context.Context, key string, fetch func(context.Context) (T, error)) (T, error) {
	cache := tickCacheFromContext(ctx)
	if cache == nil {
		return fetch(ctx)
	}
	cache.mu.Lock()
	if existing, ok := cache.values[key]; ok {
		typed := existing.(T)
		cache.mu.Unlock()
		return typed, nil
	}
	cache.mu.Unlock()

	val, err := fetch(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	cache.mu.Lock()
	cache.values[key] = val
	cache.mu.Unlock()
	return val, nil
}
