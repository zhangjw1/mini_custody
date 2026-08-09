package migrations

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

var businessTables = []string{
	"users",
	"wallet_addresses",
	"asset_balances",
	"deposits",
	"withdrawals",
	"balance_entries",
	"chain_checkpoints",
	"worker_errors",
}

type Verification struct {
	MigrationVersion int64  `json:"migration_version"`
	Tables           int    `json:"tables"`
	Columns          int    `json:"columns"`
	IdentityIDs      int    `json:"identity_ids"`
	PrimaryKeyIDs    int    `json:"primary_key_ids"`
	TableComments    int    `json:"table_comments"`
	ColumnComments   int    `json:"column_comments"`
	Timezone         string `json:"timezone"`
}

// VerifySchema 从 PostgreSQL 系统目录校验业务表、字段、主键、注释和时区。
func VerifySchema(ctx context.Context, pool *pgxpool.Pool) (Verification, error) {
	if pool == nil {
		return Verification{}, errors.New("必须提供数据库结构检查连接池")
	}
	var result Verification
	queries := []struct {
		name string
		sql  string
		dest *int
	}{
		{
			name: "业务表",
			sql: `SELECT COUNT(*) FROM information_schema.tables
				WHERE table_schema = current_schema() AND table_name = ANY($1::text[])`,
			dest: &result.Tables,
		},
		{
			name: "业务字段",
			sql: `SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = ANY($1::text[])`,
			dest: &result.Columns,
		},
		{
			name: "自增主键",
			sql: `SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = ANY($1::text[])
				  AND column_name = 'id' AND is_identity = 'YES'`,
			dest: &result.IdentityIDs,
		},
		{
			name: "主键约束",
			sql: `SELECT COUNT(DISTINCT tc.table_name)
				FROM information_schema.table_constraints tc
				JOIN information_schema.key_column_usage kcu
				  ON kcu.constraint_schema = tc.constraint_schema
				 AND kcu.constraint_name = tc.constraint_name
				WHERE tc.table_schema = current_schema()
				  AND tc.table_name = ANY($1::text[])
				  AND tc.constraint_type = 'PRIMARY KEY'
				  AND kcu.column_name = 'id'`,
			dest: &result.PrimaryKeyIDs,
		},
		{
			name: "表注释",
			sql: `SELECT COUNT(*)
				FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = current_schema()
				  AND c.relname = ANY($1::text[])
				  AND obj_description(c.oid, 'pg_class') IS NOT NULL`,
			dest: &result.TableComments,
		},
		{
			name: "字段注释",
			sql: `SELECT COUNT(*)
				FROM pg_attribute a
				JOIN pg_class c ON c.oid = a.attrelid
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = current_schema()
				  AND c.relname = ANY($1::text[])
				  AND a.attnum > 0 AND NOT a.attisdropped
				  AND col_description(c.oid, a.attnum) IS NOT NULL`,
			dest: &result.ColumnComments,
		},
	}
	for _, query := range queries {
		if err := pool.QueryRow(ctx, query.sql, businessTables).Scan(query.dest); err != nil {
			return Verification{}, fmt.Errorf("校验%s失败：%w", query.name, err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&result.MigrationVersion); err != nil {
		return Verification{}, fmt.Errorf("校验迁移版本失败：%w", err)
	}
	if err := pool.QueryRow(ctx, `SHOW TIME ZONE`).Scan(&result.Timezone); err != nil {
		return Verification{}, fmt.Errorf("校验数据库时区失败：%w", err)
	}
	if result.MigrationVersion < 1 || result.Tables != 8 || result.Columns != 79 ||
		result.IdentityIDs != 8 || result.PrimaryKeyIDs != 8 || result.TableComments != 8 ||
		result.ColumnComments != 79 || result.Timezone != "Asia/Shanghai" {
		return result, errors.New("数据库结构校验未通过")
	}
	return result, nil
}
