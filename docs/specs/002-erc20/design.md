# 002 - Sepolia ERC-20 托管钱包技术设计

## 1. 文档状态

- 对应需求：[requirements.md](./requirements.md)
- 状态：待评审
- 前置阶段：`001-evm-wallet`
- 范围：一个可配置标准 ERC-20 的充值、Gas 补充、归集和热钱包提币

## 2. 设计目标

- 复用现有 Sepolia RPC、密钥、地址、余额和交易状态机；
- 通过 Event Log 正确识别 Token 充值；
- 将用户账本与链上资金位置分离；
- 通过 Gas Station 和归集建立交易所常见资金路径；
- 热钱包统一处理 Token 提币；
- 所有 Worker 支持幂等、重试和重启恢复；
- 不破坏现有 ETH 充值和提币流程。

## 3. 核心决策

| 项目 | 决策 |
| --- | --- |
| 网络 | Ethereum Sepolia，Chain ID `11155111` |
| Token 数量 | 第一阶段只启用一个配置合约 |
| Token 类型 | 标准 ERC-20，不支持转账税和 rebasing |
| 用户地址 | 复用 `m/44'/60'/0'/0/{user_index}` |
| 平台热钱包 | 复用预留索引 `m/44'/60'/0'/0/0` |
| Gas Station | 第一阶段由平台热钱包兼任 |
| 充值扫描 | `eth_getLogs` / `FilterLogs` 扫描 `Transfer` Event |
| 充值唯一键 | network + contract + tx_hash + log_index |
| 归集来源 | 用户充值地址 |
| Token 提币来源 | 平台热钱包 |
| 用户提币 Gas | 平台承担，单独记录 ETH 运营成本 |
| 精度 | 数据库最小单位整数，API 十进制字符串 |
| 进程形态 | 继续使用一个 Go 单体进程和 PostgreSQL |

Token 合约地址不写死在代码中。真实 Sepolia 演示前单独确认测试 Token；如果第三方测试 Token 不稳定，可以部署只用于 Sepolia 的可铸造测试 Token，但业务实现仍按外部标准 ERC-20 对待。

## 4. 四类资金状态

系统必须区分以下资金概念：

1. **用户 Token 账本余额**：平台对用户的负债，充值确认后增加，提币完成后减少；
2. **用户地址链上 Token**：尚未归集的链上库存，不等同于用户可用余额；
3. **热钱包 Token 库存**：已经归集、可用于统一提币的链上 Token；
4. **平台 ETH Gas**：用于 Gas 补充、归集和 Token 提币，不属于用户 Token 余额。

```text
外部 Token
    -> 用户充值地址
    -> 充值确认并增加用户账本
    -> Gas Station 补 ETH
    -> 用户地址归集 Token
    -> 平台热钱包库存
    -> 热钱包统一 Token 提币
```

充值入账和归集是两个独立事务。归集失败只影响平台资金位置，不回滚用户已确认余额。

## 5. 模块结构

```text
Go Application
    +-- Asset Registry
    +-- ERC20 Contract Client
    +-- Token Deposit Scanner
    +-- Token Deposit Confirmer
    +-- Gas Station Worker
    +-- Sweep Worker
    +-- Token Withdrawal Service
    +-- Token Withdrawal Worker
    +-- Internal Transfer Registry
    +-- Platform Wallet Monitor
    +-- HTTP API
    |
    +--> PostgreSQL
    +--> Sepolia RPC
```

不单独引入消息队列。任务状态保存在 PostgreSQL，Worker 每轮查询可处理状态并通过行锁抢占。

## 6. ERC-20 链客户端

### 6.1 合约校验

在 `internal/chain/evm` 上扩展以下窄接口：

```go
type TokenClient interface {
    CodeAt(ctx context.Context, contract common.Address, block *big.Int) ([]byte, error)
    CallContract(ctx context.Context, call ethereum.CallMsg, block *big.Int) ([]byte, error)
    FilterLogs(ctx context.Context, query ethereum.FilterQuery) ([]types.Log, error)
    BalanceAtToken(ctx context.Context, contract, owner common.Address) (*big.Int, error)
    EstimateTokenTransferGas(ctx context.Context, contract, from, to common.Address, amount *big.Int) (uint64, error)
}
```

使用 `go-ethereum/accounts/abi` 编码和解码：

- `symbol()`；
- `decimals()`；
- `balanceOf(address)`；
- `transfer(address,uint256)`；
- `Transfer(address,address,uint256)` Event。

ABI 只保留实际使用的标准方法和事件。业务层不得自行拼接 calldata 或 topic 字符串。

### 6.2 日志扫描

每批按 `[fromBlock, toBlock]` 查询：

- `address = configured token contract`；
- `topic[0] = keccak256("Transfer(address,address,uint256)")`；
- 返回后在本地地址索引中过滤 `to`；
- 校验 topics 数量、data 长度、金额和区块字段；
- 忽略 `Removed == true` 的日志并记录安全错误。

第一阶段不把所有用户地址放入 RPC Topic OR 条件，避免公共 RPC 对地址数量和请求大小的差异限制。只有一个测试 Token 时，扫描全部 Transfer 日志后本地过滤可以接受。

## 7. 数据模型

继续使用自增 `BIGINT IDENTITY`、`TIMESTAMPTZ` 和 `NUMERIC(78,0)`。

### 7.1 `assets`

| 字段 | 说明 |
| --- | --- |
| `id` | 资产主键 |
| `network` | `ethereum-sepolia` |
| `asset_type` | `NATIVE` 或 `ERC20` |
| `symbol` | 展示代码 |
| `contract_address` | ERC-20 合约地址，原生资产为空 |
| `decimals` | 链上精度 |
| `enabled` | 是否启用 |

唯一约束：`(network, contract_address)` 和 `(network, symbol)`。

现有 `asset_balances.asset` 继续保存 symbol，迁移后增加 `asset_id` 外键；过渡期间 API 不改变 ETH 返回格式。

### 7.2 `token_deposits`

| 字段 | 说明 |
| --- | --- |
| `user_id` / `address_id` / `asset_id` | 归属 |
| `tx_hash` / `log_index` | Event 唯一位置 |
| `block_number` / `block_hash` | 区块信息 |
| `from_address` / `to_address` | Event 地址 |
| `amount_units` | Token 最小单位整数 |
| `confirmations` / `status` | 确认状态 |

唯一约束：`(asset_id, tx_hash, log_index)`。

### 7.3 `platform_wallets`

| 字段 | 说明 |
| --- | --- |
| `network` / `role` | 网络和 `HOT` 角色 |
| `address` / `derivation_path` | 平台地址信息 |
| `next_nonce` | 平台交易 Nonce |

第一阶段一个 `HOT` 地址同时承担 Gas Station 和 Token 提币。后续可以增加独立 Gas Station，不改变任务表关联方式。

### 7.4 `internal_transfers`

保存平台向用户地址发送的 Gas 补充：

- `transfer_type = GAS_TOPUP`；
- from/to、amount、nonce、raw_tx、tx_hash；
- Gas 参数、Receipt、实际费用和状态；
- 关联 sweep 任务。

已签名交易必须在广播前落库。现有 ETH 充值扫描器在创建充值前查询 `tx_hash` 是否属于内部转账；匹配时只推进区块检查点，不增加用户 ETH 余额。

### 7.5 `token_sweeps`

保存 Token 归集任务：

- user/address/asset；
- 触发充值和归集金额；
- Gas 补充任务；
- nonce、raw_tx、tx_hash；
- Gas 参数、Receipt、实际费用；
- 状态和错误。

同一地址同一资产只允许一笔处于执行状态的归集。新充值可以合并进尚未签名的归集；已经签名后到达的新 Token 创建下一笔任务。

### 7.6 `token_withdrawals`

保存用户 Token 提币：

- user/asset/idempotency_key；
- 热钱包 ID 和目标地址；
- Token 最小单位金额；
- 热钱包 nonce、raw_tx、tx_hash；
- EIP-1559 参数、Receipt 和实际 ETH Gas；
- 状态和错误。

唯一约束：`(user_id, idempotency_key)`。

### 7.7 检查点与流水

- Token 扫描检查点使用 `scanner = erc20:<contract_address>`；
- Token 充值流水使用 `reference_type = TOKEN_DEPOSIT`；
- Token 提币流水使用 `reference_type = TOKEN_WITHDRAWAL`；
- 归集和 Gas 补充是平台链上资金移动，不改变用户账本，不写用户余额流水。

## 8. Token 充值流程

### 8.1 扫描事务

对于每批日志：

1. 按区块和日志索引稳定排序；
2. 校验合约、Event 签名、目标地址和金额；
3. 插入新的 `token_deposits`；
4. 新记录增加 `pending_deposit` 并追加唯一待确认流水；
5. 保存批次最后完整区块的检查点；
6. 以上操作在一个数据库事务中完成。

RPC 返回错误、日志字段无效或数据库事务失败时，检查点不得推进。

### 8.2 确认入账

达到确认数后复核原区块哈希，在一个事务中：

1. Token 充值变为 `CONFIRMED`；
2. 从用户 Token 待确认余额转入可用余额；
3. 写入唯一 `DEPOSIT_CREDIT` 流水；
4. 充值变为 `CREDITED`；
5. 幂等创建归集任务。

## 9. Gas Station 与内部 ETH

### 9.1 补充金额

```text
required = estimated_gas_limit * max_fee_per_gas
topup = max(0, required + safety_margin - user_address_eth_balance)
```

安全余量使用配置比例并设置绝对上限。不能向用户地址长期堆积大量 ETH。

### 9.2 执行顺序

1. 锁定 sweep 和平台热钱包 Nonce；
2. 构建并签名普通 EIP-1559 ETH 转账；
3. 在 `internal_transfers` 保存 raw_tx 和 tx_hash；
4. 广播相同 raw_tx；
5. ETH 扫描器根据 tx_hash 跳过用户入账；
6. Gas 补充确认后推进 sweep；
7. 失败和不明确广播沿用现有恢复策略。

内部转账登记必须先于广播，避免扫描器在高速出块或 RPC 延迟下先观察到交易。

## 10. Token 归集

归集调用：

```text
token.transfer(platform_hot_wallet, sweep_amount)
```

签名前执行：

- 读取用户地址 Token 余额；
- 读取用户地址 ETH 余额；
- 估算 transfer Gas；
- 分配用户地址 Nonce；
- 校验 sweep 金额大于零且不超过可归集金额；
- 校验目标为配置热钱包。

归集确认后记录实际 Gas 和热钱包库存快照。库存快照用于展示，不作为唯一账本；需要时必须重新读取链上 `balanceOf(hot_wallet)`。

## 11. Token 提币

### 11.1 创建与占用

API 将十进制 Token 金额转成最小单位，在事务中锁定用户 Token 余额：

```text
available_token -> pending_withdrawal_token
```

平台 Gas 不从用户 Token 余额扣除。创建时仍返回预估 ETH Gas，作为后台运营信息。

### 11.2 热钱包出款

Worker 处理前检查：

- 热钱包 `balanceOf(token) >= amount`；
- 热钱包 ETH 足够支付最大 Gas；
- 目标地址有效且不等于零地址；
- calldata 解码结果等于保存的目标和金额。

平台热钱包 Nonce 在 `platform_wallets` 行锁内分配。Gas 补充和 Token 提币共享同一个 Nonce 序列。

### 11.3 结算

- Receipt 成功：扣除用户 pending Token，写 `WITHDRAW_FINALIZE`，保存实际 ETH Gas；
- Receipt 失败：Token 金额返回用户 available，写 `WITHDRAW_RELEASE`，保存实际 ETH Gas；
- Receipt 未找到或 RPC 超时：保持可恢复状态，不释放用户余额。

## 12. Worker 调度与并发

- 每类 Worker 只查询明确的可处理状态；
- 数据库更新带状态条件和 `FOR UPDATE SKIP LOCKED`；
- 平台热钱包 Nonce 通过单行锁串行分配；
- 用户地址 Nonce 通过现有 `wallet_addresses.next_nonce` 分配；
- Worker 获取任务后仍需重新读取最新记录；
- 单条失败记录 `worker_errors`，不阻塞同批其他任务；
- 多实例部署前增加 PostgreSQL Advisory Lock，MVP 可以先保持单实例。

## 13. API 设计

新增或扩展：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/api/v1/assets` | ETH 和启用 Token 列表 |
| `GET` | `/api/v1/users/{id}/balances` | 用户多资产余额 |
| `GET` | `/api/v1/users/{id}/token-deposits` | Token 充值记录 |
| `GET` | `/api/v1/users/{id}/token-withdrawals` | Token 提币记录 |
| `POST` | `/api/v1/users/{id}/token-withdrawal-quote` | Token 提币试算 |
| `POST` | `/api/v1/users/{id}/token-withdrawals` | 创建 Token 提币 |
| `GET` | `/api/v1/token-withdrawals/{id}` | 查询 Token 提币 |
| `GET` | `/api/v1/sweeps` | 归集任务列表 |
| `GET` | `/api/v1/internal-transfers` | Gas 补充列表 |
| `GET` | `/api/v1/system/platform-wallet` | 热钱包库存和 Gas 状态 |

金额响应同时提供 `amount_units` 和格式化 `amount`，不得返回 JSON Number。

## 14. Web 设计

- 顶部增加资产选择器，用户选择器保持不变；
- Dashboard 墕加热钱包 Token 库存、ETH Gas 和待归集数量；
- Account 使用资产表格展示三类余额；
- Deposit 根据资产展示同一地址，并显示 Token 合约信息；
- Withdraw 根据资产调用不同试算和创建接口；
- Transactions 增加 Token、Sweep 和 Gas Top-up 类型筛选；
- 新增 Operations 页面查看归集和内部转账失败；
- 只展示必要运维数据，不提供密钥、签名或任意交易操作。

## 15. 配置

```text
ERC20_ENABLED=true
ERC20_CONTRACT_ADDRESS=0x...
ERC20_SYMBOL=...
ERC20_DECIMALS=...
ERC20_SCAN_START_BLOCK=...
ERC20_SCAN_BATCH_SIZE=...
ERC20_CONFIRMATIONS=...
ERC20_SWEEP_ENABLED=true
ERC20_SWEEP_INTERVAL=...
ERC20_GAS_SAFETY_BPS=...
ERC20_GAS_TOPUP_MAX_WEI=...
PLATFORM_HOT_WALLET_PATH=m/44'/60'/0'/0/0
PLATFORM_MIN_ETH_BALANCE_WEI=...
```

合约地址、symbol 和 decimals 不是秘密，可以写入部署配置；RPC URL、密钥密码和根密钥继续只通过本地环境或秘密管理系统注入。

## 16. 测试策略

### 16.1 单元测试

- ERC-20 ABI 编解码和 Event 解析；
- decimals 精确换算与边界；
- 日志唯一性和排序；
- Gas 补充金额和上限；
- 内部 ETH 转账排除；
- sweep 状态机和重启恢复；
- 平台热钱包 Nonce 并发；
- Token 提币余额占用、失败释放和幂等。

### 16.2 Mock RPC 契约测试

- `eth_getCode`、`eth_call`、`eth_getLogs`；
- 日志分页、空结果、429、超时和错误区块；
- transfer Gas 估算、广播不明确和 Receipt；
- 合约返回 `false`、revert 和无效 ABI 数据。

### 16.3 PostgreSQL 集成测试

- Token 充值和检查点原子提交；
- 重复日志不重复入账；
- Gas 补充不产生 ETH 用户余额；
- 同一地址只有一个活动 sweep；
- 热钱包 Nonce 不重复；
- Token 提币并发不能超花。

### 16.4 Sepolia 端到端

通过显式环境开关运行：

1. 验证 Token 合约元数据；
2. 给 Alice 地址发送测试 Token；
3. 等待充值确认和用户账本入账；
4. 等待 Gas 补充和归集；
5. 从 Web 创建 Token 提币；
6. 验证热钱包出款、Receipt、实际 Gas 和最终账本；
7. 在广播阶段重启，验证没有重复交易。

普通测试不会部署合约、请求水龙头或连接公开 RPC。

## 17. 风险与边界

| 风险 | 本阶段处理 |
| --- | --- |
| 测试 Token 合约不稳定 | 合约可配置，启动时校验代码和元数据 |
| 公共 RPC 限制日志范围 | 小批量扫描、退避、持久化检查点 |
| Gas 补充被误判为充值 | 广播前登记内部 tx_hash，ETH 扫描器排除 |
| Token 有转账税或 rebasing | 明确不支持，只接受标准测试 Token |
| 热钱包 Token 库存不足 | 暂停提币并告警，不修改用户账本 |
| 热钱包成为集中风险 | 仅测试网；后续拆签名服务、限额和冷钱包 |
| 同一热钱包 Nonce 竞争 | 所有平台交易共享数据库 Nonce 行锁 |
| 归集失败 | 保留用户账本，任务持续可见和可恢复 |

## 18. 后续演进

- 多 Token 资产注册和动态 Worker；
- 独立 Gas Station 地址；
- 提币手续费和 Token 计价；
- 热钱包限额、冷钱包和审批；
- 对账、自动链重组恢复和多实例租约；
- 将相同资产与账本接口复用于 Solana SPL Token。
