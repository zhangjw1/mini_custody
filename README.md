# Mini Custody

Mini Custody 是一个面向学习和求职展示的多链托管钱包项目。当前稳定阶段已经实现 Ethereum Sepolia 原生 ETH 的地址派生、充值扫描和提币闭环；下一阶段规划 Sepolia ERC-20 充值、Gas 补充、归集和热钱包统一提币。

## 当前阶段

开发任务见：

- `docs/specs/001-evm-wallet/requirements.md`
- `docs/specs/001-evm-wallet/design.md`
- `docs/specs/001-evm-wallet/tasks.md`
- `docs/specs/002-erc20/requirements.md`
- `docs/specs/002-erc20/design.md`
- `docs/specs/002-erc20/tasks.md`
- `docs/development/coding-conventions.md`
- `docs/api.md`
- `docs/testing/sepolia-e2e.md`
- `docs/testing/phase-8-evidence.md`

## 基础环境

- Go 1.23.5
- PostgreSQL 17+
- Ethereum Sepolia RPC

项目仅允许使用测试网资产。不要向项目生成的地址转入主网或其他真实资产。

## 后端启动

项目后端是单体 Go Web 应用。`cmd/api` 会在启动时连接 PostgreSQL、自动执行数据库迁移、加载托管根密钥并初始化演示用户。

```bash
docker compose -f deploy/compose.yaml up -d postgres
cd backend
# 将真实配置写入 ../deploy/config.local.yaml（该文件已被 Git 忽略）
export CONFIG_FILE=../deploy/config.local.yaml
make run
```

健康检查：`GET http://localhost:8080/healthz`。

## Web 后台启动

Web 后台位于同一仓库的 `frontend/` 目录。开发环境通过 Vite 将 `/api` 代理到 `http://127.0.0.1:8080`，因此需要先启动后端。

```bash
cd frontend
npm install
npm run dev
```

访问 `http://127.0.0.1:5173/`。页面包含资产总览、托管账户、充值二维码、提币试算与提交、全局资金流水，不直接连接 Sepolia RPC，也不接触托管密钥。

提交前端代码前执行：

```bash
npm run typecheck
npm run build
```

创建 Sepolia ETH 提币：

```bash
curl -X POST http://localhost:8080/api/v1/users/1/withdrawals \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: 自定义且不可重复的业务标识' \
  -d '{"to_address":"0x目标地址","amount_eth":"0.001"}'
```

金额必须使用十进制字符串，最多 18 位小数。相同用户和 `Idempotency-Key` 的重试返回原提币，不会创建第二笔链上交易。

`cmd/walletgen` 只用于离线生成和验证加密根密钥；`cmd/migrate` 是可选的数据库运维命令，不是独立服务。

配置专用 PostgreSQL 测试库后，可以运行真实事务和并发测试：

```bash
CONFIG_FILE=../deploy/config.local.yaml make integration-test
```

## Sepolia 端到端验收

普通 `make test` 不连接公开 RPC，也不会创建链上交易。Phase 8 提供独立的只读验收目标，用指定的历史充值和提币交易校验 API、PostgreSQL 账本与 Sepolia Receipt 三方一致：

```bash
cd backend
export CONFIG_FILE=../deploy/config.local.yaml
export E2E_EXPECTED_WALLET_ADDRESS='0x...'
export E2E_DEPOSIT_TX_HASH='0x...'
export E2E_WITHDRAWAL_TX_HASH='0x...'
make e2e-test
```

执行真实 USDC 流程前，先运行只读环境预检。它只查询 RPC、合约余额和数据库，不签名、不广播交易：

```bash
cd backend
export CONFIG_FILE=../deploy/config.local.yaml
export E2E_EXTERNAL_ADDRESS='0x外部测试地址'
make erc20-preflight
```

完整的测试密钥、RPC、测试币准备方式和 A-E 场景演示步骤见 `docs/testing/sepolia-e2e.md`。仓库中的 `deploy/env.example` 只保留变量名和非敏感默认值；真实值应放在已被 Git 忽略的 `deploy/config.local.yaml`。
