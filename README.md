# Mini Custody

Mini Custody 是一个面向学习和求职展示的多链托管钱包项目。当前阶段实现 Ethereum Sepolia 原生 ETH 的地址派生、充值扫描和提币闭环。

## 当前阶段

开发任务见：

- `docs/specs/001-evm-wallet/requirements.md`
- `docs/specs/001-evm-wallet/design.md`
- `docs/specs/001-evm-wallet/tasks.md`

## 基础环境

- Go 1.23.5
- PostgreSQL 17+
- Ethereum Sepolia RPC

项目仅允许使用测试网资产。不要向项目生成的地址转入主网或其他真实资产。

