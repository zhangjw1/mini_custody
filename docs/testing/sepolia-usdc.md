# Sepolia USDC 测试资产

## 1. 固定信息

| 项目 | 值 |
|---|---|
| 网络 | Ethereum Sepolia |
| Chain ID | `11155111` |
| Token | Circle Testnet USDC |
| 合约地址 | `0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238` |
| Symbol | `USDC` |
| Decimals | `6` |
| 最小单位 | `0.000001 USDC` |

资料来源：

- [Circle 官方 USDC 合约地址](https://developers.circle.com/stablecoins/usdc-contract-addresses)
- [Circle 官方测试币 Faucet](https://faucet.circle.com/)
- [Sepolia Etherscan Token 页面](https://sepolia.etherscan.io/token/0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238)

测试网 USDC 没有真实金融价值，只用于开发和验收。

## 2. 获取测试币

1. 准备一个 Sepolia 地址；
2. 打开 Circle Faucet，网络选择 Ethereum Sepolia；
3. 资产选择 USDC，并填写接收地址；
4. Faucet 发送后，在 Etherscan Token 页面确认 `Transfer` Event。

Faucet 的限额和领取间隔由 Circle 控制，测试代码不能依赖 Faucet 实时可用。

## 3. 项目配置

`deploy/env.example` 已提供非敏感默认值。首次启用 ERC-20 扫描前，应把
`ERC20_SCAN_START_BLOCK` 设置为部署时记录的安全区块高度；后续重启优先从数据库检查点继续。

```text
ERC20_ENABLED=true
ERC20_CONTRACT_ADDRESS=0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238
ERC20_SYMBOL=USDC
ERC20_DECIMALS=6
ERC20_SWEEP_ENABLED=true
E2E_EXTERNAL_ADDRESS=0x外部测试地址
```

`E2E_EXTERNAL_ADDRESS` 只填写公开地址，不能填写私钥或助记词。该地址用于向系统充值，必须同时准备：

- 大于零的 Sepolia ETH，用于支付一次 USDC 充值交易的 Gas；
- 足够完成一次小额充值的 Sepolia USDC。

平台热钱包也必须同时持有：

- 不低于 `PLATFORM_MIN_ETH_BALANCE_WEI` 的 Sepolia ETH，用于 Gas 补充、归集和 Token 提币；
- 足够完成一次小额 Token 提币的 Sepolia USDC 库存。

## 4. 只读环境预检

预检命令只执行 RPC 查询和 PostgreSQL 查询，不加载托管密钥、不签名、不广播交易，也不修改数据库：

```bash
cd backend
set -a
source ../deploy/.env.local
set +a
make erc20-preflight
```

也可以临时指定外部地址并输出 JSON：

```bash
CGO_ENABLED=0 go run ./cmd/erc20-preflight \
  --external-address 0x外部测试地址 \
  --json
```

预检逐项确认：

1. `ERC20_ENABLED` 和 `ERC20_SWEEP_ENABLED` 已开启；
2. RPC Chain ID 为 `11155111` 且能读取最新区块；
3. 合约存在字节码，链上 `symbol()` 和 `decimals()` 与配置一致；
4. PostgreSQL 中 USDC 资产已启用且合约配置一致；
5. 平台热钱包记录存在，ETH 达到配置阈值且 USDC 库存大于零；
6. 外部测试地址 ETH 和 USDC 余额均大于零。

任何关键项失败时命令都会打印所有已完成的检查并以非零状态退出。预检通过只代表环境具备测试条件，不会自动开始充值、归集或提币。

## 5. 真实交易安全开关

后续真实 Sepolia ERC-20 E2E 必须显式设置以下开关，普通单元测试和预检命令不读取该开关，也永远不会广播交易：

```bash
export E2E_SEPOLIA=1
```

每次真实验收前都要先运行预检。测试完成后执行 `unset E2E_SEPOLIA`，避免后续终端命令误进入真实交易流程。

本项目不部署自定义 Demo Token。若 Circle 后续迁移或停用该测试合约，需要重新评审合约代码、
`symbol()`、`decimals()` 和测试币来源后再修改配置。
