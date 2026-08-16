package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v5"
	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

// EnsureERC20Asset 幂等登记单个 ERC-20 资产并为现有用户初始化零余额行。
func (s *Store) EnsureERC20Asset(ctx context.Context, configured Asset) (Asset, error) {
	configured.Network = strings.TrimSpace(configured.Network)
	configured.AssetType = AssetTypeERC20
	configured.Symbol = strings.ToUpper(strings.TrimSpace(configured.Symbol))
	configured.ContractAddress = strings.ToLower(strings.TrimSpace(configured.ContractAddress))
	if configured.Network != NetworkSepolia || configured.Symbol == "" || configured.Decimals == 0 || configured.Decimals > 18 ||
		!common.IsHexAddress(configured.ContractAddress) || common.HexToAddress(configured.ContractAddress) == (common.Address{}) {
		return Asset{}, errors.New("ERC-20 资产初始化参数无效")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Asset{}, fmt.Errorf("开启 ERC-20 资产初始化事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO assets (network, asset_type, symbol, contract_address, decimals, enabled)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING`,
		configured.Network, configured.AssetType, configured.Symbol, configured.ContractAddress,
		configured.Decimals, configured.Enabled,
	); err != nil {
		return Asset{}, fmt.Errorf("写入 ERC-20 资产失败：%w", err)
	}

	item, err := s.scanAsset(tx.QueryRow(ctx,
		`SELECT `+assetColumns+` FROM assets WHERE network = $1 AND symbol = $2 FOR UPDATE`,
		configured.Network, configured.Symbol,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrAssetConfigMismatch
	}
	if err != nil {
		return Asset{}, fmt.Errorf("读取 ERC-20 资产失败：%w", err)
	}
	if item.AssetType != AssetTypeERC20 || item.ContractAddress != configured.ContractAddress || item.Decimals != configured.Decimals {
		return Asset{}, ErrAssetConfigMismatch
	}
	if item.Enabled != configured.Enabled {
		item, err = s.scanAsset(tx.QueryRow(ctx, `
			UPDATE assets SET enabled = $2, updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 RETURNING `+assetColumns, item.ID, configured.Enabled,
		))
		if err != nil {
			return Asset{}, fmt.Errorf("更新 ERC-20 启用状态失败：%w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO asset_balances (user_id, asset_id, asset)
		SELECT id, $1, $2 FROM users
		ON CONFLICT (user_id, asset_id) DO NOTHING`, item.ID, item.Symbol,
	); err != nil {
		return Asset{}, fmt.Errorf("初始化用户 ERC-20 余额失败：%w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Asset{}, fmt.Errorf("提交 ERC-20 资产初始化事务失败：%w", err)
	}
	return item, nil
}

// EnsurePlatformHotWallet 幂等派生并登记平台 HOT 钱包，同时校验数据库与根密钥一致。
func (s *Store) EnsurePlatformHotWallet(ctx context.Context, provider wallet.KeyProvider, derivationPath string) (PlatformWallet, error) {
	if provider == nil {
		return PlatformWallet{}, errors.New("初始化平台热钱包必须提供密钥提供器")
	}
	derivationPath = strings.TrimSpace(derivationPath)
	if derivationPath != "m/44'/60'/0'/0/0" {
		return PlatformWallet{}, errors.New("平台热钱包派生路径无效")
	}
	derivedAddress, err := provider.Address(ctx, derivationPath)
	if err != nil {
		return PlatformWallet{}, fmt.Errorf("派生平台热钱包地址失败：%w", err)
	}
	normalizedAddress := strings.ToLower(derivedAddress.Hex())

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return PlatformWallet{}, fmt.Errorf("开启平台热钱包初始化事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO platform_wallets (network, role, address, derivation_path)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (network, role) DO NOTHING`,
		NetworkSepolia, PlatformRoleHot, normalizedAddress, derivationPath,
	); err != nil {
		return PlatformWallet{}, fmt.Errorf("写入平台热钱包失败：%w", err)
	}
	item, err := s.scanPlatformWallet(tx.QueryRow(ctx, `
		SELECT `+platformWalletColumns+` FROM platform_wallets
		WHERE network = $1 AND role = $2 FOR UPDATE`, NetworkSepolia, PlatformRoleHot,
	))
	if err != nil {
		return PlatformWallet{}, fmt.Errorf("读取平台热钱包失败：%w", err)
	}
	if item.Address != normalizedAddress || item.DerivationPath != derivationPath {
		return PlatformWallet{}, ErrWalletKeyMismatch
	}
	if err := tx.Commit(ctx); err != nil {
		return PlatformWallet{}, fmt.Errorf("提交平台热钱包初始化事务失败：%w", err)
	}
	return item, nil
}
