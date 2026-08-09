# Mini Custody 开发约定

## 1. 注释和消息

- 每个 Go 函数、方法、接口方法、测试方法和测试辅助函数都必须有中文注释。
- 注释需要说明函数的职责或关键约束，不能只是重复函数名。
- 运行日志、命令行提示和向上返回的错误必须使用中文。
- 环境变量、JSON 字段、数据库字段、链上状态、稳定错误码等机器接口保持英文，避免破坏协议兼容性。
- 底层错误使用 `%w` 保留错误链，禁止只记录中文后丢失原始原因。

`internal/style` 中的 AST 测试会自动检查函数注释和运行时消息。

## 2. 数据库技术栈

当前数据库访问使用 `github.com/jackc/pgx/v5`：

- `pgxpool` 管理 PostgreSQL 连接池；
- Repository 使用显式 SQL；
- 余额事务使用 PostgreSQL `FOR UPDATE` 行锁、唯一约束和事务；
- 资产金额使用 `NUMERIC(78,0)` 和 Go `big.Int`；
- 当前没有使用 GORM、Ent 等 ORM。

托管钱包的余额、幂等和 Nonce 逻辑依赖精确的 SQL 与锁顺序。显式 SQL 比 ORM 隐式生成语句更容易审查事务边界，也更容易验证是否真正使用了行锁。

## 3. 数据库迁移和 embed

迁移器是项目内的轻量迁移实现，不是第三方迁移框架。它提供：

- `schema_migrations` 版本记录；
- PostgreSQL Advisory Lock，避免多个实例并发迁移；
- 按版本执行 `up/down` SQL；
- 单迁移事务；
- 数据库结构只读检查。

Go 标准库 `embed` 通过下面的指令把 SQL 文件编译进 Go 二进制：

```go
//go:embed *.up.sql *.down.sql
var migrationFiles embed.FS
```

因此部署时不需要额外复制 `migrations/*.sql`，程序从任意工作目录启动都能读取完全相同的迁移文件，也不会因为容器漏打包 SQL 而启动失败。
