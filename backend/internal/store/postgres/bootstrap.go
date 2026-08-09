package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/xiaoqi/mini-custody/backend/internal/wallet"
)

type demoUser struct {
	code        string
	displayName string
	index       uint32
}

var demoUsers = []demoUser{
	{code: "demo-alice", displayName: "Alice", index: 1},
	{code: "demo-bob", displayName: "Bob", index: 2},
}

// BootstrapDemoUsers 幂等创建演示用户、确定性钱包地址和初始余额行。
func (s *Store) BootstrapDemoUsers(ctx context.Context, provider wallet.KeyProvider) error {
	if provider == nil {
		return fmt.Errorf("初始化演示用户失败：必须提供密钥提供器")
	}
	type derivedDemoUser struct {
		demoUser
		path    string
		address string
	}
	derived := make([]derivedDemoUser, 0, len(demoUsers))
	for _, definition := range demoUsers {
		path := wallet.UserPath(definition.index)
		address, err := provider.Address(ctx, path)
		if err != nil {
			return fmt.Errorf("为用户 %s 派生钱包失败：%w", definition.code, err)
		}
		derived = append(derived, derivedDemoUser{
			demoUser: definition,
			path:     path,
			address:  strings.ToLower(address.Hex()),
		})
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启演示用户初始化事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, definition := range derived {
		var userID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (code, display_name)
			VALUES ($1, $2)
			ON CONFLICT (code) DO UPDATE SET display_name = EXCLUDED.display_name
			RETURNING id`, definition.code, definition.displayName).Scan(&userID); err != nil {
			return fmt.Errorf("写入演示用户 %s 失败：%w", definition.code, err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO wallet_addresses (
				user_id, network, address, derivation_index, derivation_path
			) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, network) DO NOTHING`,
			userID, NetworkSepolia, definition.address, definition.index, definition.path,
		); err != nil {
			return fmt.Errorf("写入用户 %s 的钱包地址失败：%w", definition.code, err)
		}

		var storedAddress, storedPath string
		var storedIndex int64
		if err := tx.QueryRow(ctx, `
			SELECT address, derivation_index, derivation_path
			FROM wallet_addresses
			WHERE user_id = $1 AND network = $2
			FOR UPDATE`, userID, NetworkSepolia).Scan(&storedAddress, &storedIndex, &storedPath); err != nil {
			return fmt.Errorf("校验用户 %s 的钱包地址失败：%w", definition.code, err)
		}
		if storedAddress != definition.address || storedIndex != int64(definition.index) || storedPath != definition.path {
			return fmt.Errorf("%w：用户 %s", ErrWalletKeyMismatch, definition.code)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO asset_balances (user_id, asset)
			VALUES ($1, $2)
			ON CONFLICT (user_id, asset) DO NOTHING`, userID, AssetETH,
		); err != nil {
			return fmt.Errorf("初始化用户 %s 的资产余额失败：%w", definition.code, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交演示用户初始化事务失败：%w", err)
	}
	return nil
}
