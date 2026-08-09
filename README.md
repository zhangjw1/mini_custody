# Mini Custody

Mini Custody 是一个面向学习和求职展示的多链托管钱包项目。当前阶段实现 Ethereum Sepolia 原生 ETH 的地址派生、充值扫描和提币闭环。

## 当前阶段

开发任务见：

- `docs/specs/001-evm-wallet/requirements.md`
- `docs/specs/001-evm-wallet/design.md`
- `docs/specs/001-evm-wallet/tasks.md`
- `docs/development/coding-conventions.md`

## 基础环境

- Go 1.23.5
- PostgreSQL 17+
- Ethereum Sepolia RPC

项目仅允许使用测试网资产。不要向项目生成的地址转入主网或其他真实资产。

## 后端启动

项目后端是单体 Go Web 应用。`cmd/api` 会在启动时连接 PostgreSQL、自动执行数据库迁移、加载托管根密钥并初始化演示用户。

```bash
cd backend
cp ../deploy/env.example .env
# 将 .env 中的数据库、测试网 RPC 和测试密钥配置注入当前 Shell 后执行：
make run
```

健康检查：`GET http://localhost:8080/healthz`。

`cmd/walletgen` 只用于离线生成和验证加密根密钥；`cmd/migrate` 是可选的数据库运维命令，不是独立服务。

配置专用 PostgreSQL 测试库后，可以运行真实事务和并发测试：

```bash
TEST_DATABASE_URL='postgres://...' make integration-test
```
