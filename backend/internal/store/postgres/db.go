package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var schemaNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// PoolConfig 描述 PostgreSQL 连接池容量和连接生命周期配置。
type PoolConfig struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

// Open 创建使用默认 Schema 的 PostgreSQL 连接池。
func Open(ctx context.Context, databaseURL string, timezone *time.Location) (*pgxpool.Pool, error) {
	return open(ctx, databaseURL, timezone, "", PoolConfig{})
}

// OpenWithConfig 创建应用指定连接池参数的 PostgreSQL 连接池。
func OpenWithConfig(ctx context.Context, databaseURL string, timezone *time.Location, poolCfg PoolConfig) (*pgxpool.Pool, error) {
	return open(ctx, databaseURL, timezone, "", poolCfg)
}

// OpenInSchema 创建限定到指定 Schema 的 PostgreSQL 连接池。
func OpenInSchema(ctx context.Context, databaseURL string, timezone *time.Location, schema string) (*pgxpool.Pool, error) {
	if !schemaNamePattern.MatchString(schema) {
		return nil, errors.New("数据库 Schema 名称无效")
	}
	return open(ctx, databaseURL, timezone, schema, PoolConfig{})
}

// open 解析数据库配置、设置连接池参数和 Session 时区并验证连接。
func open(ctx context.Context, databaseURL string, timezone *time.Location, schema string, poolCfg PoolConfig) (*pgxpool.Pool, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("必须配置数据库连接地址")
	}
	if timezone == nil {
		return nil, errors.New("必须配置数据库时区")
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("解析数据库配置失败：%w", err)
	}
	if poolCfg.MaxConns > 0 {
		config.MaxConns = poolCfg.MaxConns
	}
	if poolCfg.MinConns >= 0 && (poolCfg.MaxConns == 0 || poolCfg.MinConns <= poolCfg.MaxConns) {
		config.MinConns = poolCfg.MinConns
	}
	if poolCfg.MaxConnLifetime > 0 {
		config.MaxConnLifetime = poolCfg.MaxConnLifetime
	}
	if poolCfg.MaxConnIdleTime > 0 {
		config.MaxConnIdleTime = poolCfg.MaxConnIdleTime
	}
	if schema != "" {
		config.ConnConfig.RuntimeParams["search_path"] = schema
	}
	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, `SELECT set_config('TimeZone', $1, false)`, timezone.String()); err != nil {
			return fmt.Errorf("设置数据库时区失败：%w", err)
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("创建数据库连接池失败：%w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库失败：%w", err)
	}
	return pool, nil
}
