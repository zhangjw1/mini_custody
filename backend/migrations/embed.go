package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.up.sql *.down.sql
var migrationFiles embed.FS

const migrationLockID int64 = 0x4d494e4943555354

var migrationName = regexp.MustCompile(`^(\d+)_.*\.(up|down)\.sql$`)

type migration struct {
	version int64
	up      string
	down    string
}

type Runner struct {
	pool *pgxpool.Pool
}

// NewRunner 创建数据库迁移执行器。
func NewRunner(pool *pgxpool.Pool) *Runner {
	return &Runner{pool: pool}
}

// Up 按版本顺序执行所有尚未应用的升级迁移。
func (r *Runner) Up(ctx context.Context) error {
	return r.withLock(ctx, func(conn *pgxpool.Conn) error {
		migrations, err := loadMigrations()
		if err != nil {
			return err
		}
		applied, err := appliedVersions(ctx, conn)
		if err != nil {
			return err
		}
		for _, item := range migrations {
			if applied[item.version] {
				continue
			}
			if err := applyFile(ctx, conn, item.version, item.up, true); err != nil {
				return err
			}
		}
		return nil
	})
}

// Down 回退当前数据库的最后一个迁移版本。
func (r *Runner) Down(ctx context.Context) error {
	return r.withLock(ctx, func(conn *pgxpool.Conn) error {
		migrations, err := loadMigrations()
		if err != nil {
			return err
		}
		version, err := currentVersion(ctx, conn)
		if err != nil {
			return err
		}
		if version == 0 {
			return nil
		}
		for _, item := range migrations {
			if item.version == version {
				return applyFile(ctx, conn, item.version, item.down, false)
			}
		}
		return fmt.Errorf("缺少版本 %d 的回退迁移", version)
	})
}

// Version 查询数据库当前迁移版本。
func (r *Runner) Version(ctx context.Context) (int64, error) {
	var version int64
	err := r.withLock(ctx, func(conn *pgxpool.Conn) error {
		var err error
		version, err = currentVersion(ctx, conn)
		return err
	})
	return version, err
}

// withLock 使用 PostgreSQL Advisory Lock 串行执行迁移操作。
func (r *Runner) withLock(ctx context.Context, fn func(*pgxpool.Conn) error) error {
	if r == nil || r.pool == nil {
		return errors.New("必须提供迁移数据库连接池")
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("获取迁移数据库连接失败：%w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("获取数据库迁移锁失败：%w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version BIGINT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return fmt.Errorf("创建迁移版本表失败：%w", err)
	}
	comments := []string{
		`COMMENT ON TABLE schema_migrations IS '数据库迁移版本记录表'`,
		`COMMENT ON COLUMN schema_migrations.version IS '已经成功应用的迁移版本号'`,
		`COMMENT ON COLUMN schema_migrations.applied_at IS '迁移成功应用时间'`,
	}
	for _, statement := range comments {
		if _, err := conn.Exec(ctx, statement); err != nil {
			return fmt.Errorf("写入迁移版本表注释失败：%w", err)
		}
	}
	return fn(conn)
}

// loadMigrations 从编译进程序的 SQL 文件加载并排序迁移版本。
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return nil, fmt.Errorf("读取内嵌迁移文件失败：%w", err)
	}
	byVersion := make(map[int64]*migration)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		parts := migrationName.FindStringSubmatch(entry.Name())
		if parts == nil {
			continue
		}
		version, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("迁移文件 %s 的版本号无效", entry.Name())
		}
		item := byVersion[version]
		if item == nil {
			item = &migration{version: version}
			byVersion[version] = item
		}
		if parts[2] == "up" {
			item.up = entry.Name()
		} else {
			item.down = entry.Name()
		}
	}

	items := make([]migration, 0, len(byVersion))
	for _, item := range byVersion {
		if item.up == "" || item.down == "" {
			return nil, fmt.Errorf("迁移版本 %d 必须同时包含升级和回退文件", item.version)
		}
		items = append(items, *item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].version < items[j].version })
	return items, nil
}

// appliedVersions 查询数据库已经应用的全部迁移版本。
func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[int64]bool, error) {
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("查询迁移版本失败：%w", err)
	}
	defer rows.Close()

	applied := make(map[int64]bool)
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("读取迁移版本失败：%w", err)
		}
		applied[version] = true
	}
	return applied, rows.Err()
}

// currentVersion 查询数据库当前最高迁移版本。
func currentVersion(ctx context.Context, conn *pgxpool.Conn) (int64, error) {
	var version int64
	if err := conn.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("查询当前迁移版本失败：%w", err)
	}
	return version, nil
}

// applyFile 在单个数据库事务中应用一个升级或回退 SQL 文件。
func applyFile(ctx context.Context, conn *pgxpool.Conn, version int64, name string, up bool) error {
	sqlBytes, err := migrationFiles.ReadFile(name)
	if err != nil {
		return fmt.Errorf("读取迁移文件 %s 失败：%w", name, err)
	}
	if strings.TrimSpace(string(sqlBytes)) == "" {
		return fmt.Errorf("迁移文件 %s 不能为空", name)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启迁移版本 %d 的事务失败：%w", version, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Conn().PgConn().Exec(ctx, string(sqlBytes)).ReadAll(); err != nil {
		return fmt.Errorf("执行迁移文件 %s 失败：%w", name, err)
	}
	if up {
		_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
	}
	if err != nil {
		return fmt.Errorf("记录迁移版本 %d 失败：%w", version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交迁移版本 %d 失败：%w", version, err)
	}
	return nil
}
