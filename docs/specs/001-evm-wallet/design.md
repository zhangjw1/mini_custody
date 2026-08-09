# 001 - Sepolia 原生 ETH 托管钱包技术设计

## 1. 文档状态

- 对应需求：[requirements.md](./requirements.md)
- 状态：待评审
- 范围：第一阶段 Sepolia 原生 ETH 充提闭环
- 不包含：ERC-20、Solana、Bitcoin、自动归集、权限、审核和主网

## 2. 设计目标

本方案需要以尽量少的通用基础设施实现以下区块链核心能力：

- 从统一根密钥确定性派生用户地址；
- 从 Sepolia 扫描用户的直接 ETH 充值；
- 按确认数幂等入账；
- 从用户托管地址构建 EIP-1559 提币交易；
- 安全处理 Nonce、签名、广播、重试和确认；
- 在公开 RPC 限流、超时和服务重启时保持数据一致；
- 通过 Web 页面观察真实链上状态。

第一阶段优先建立正确的链上模型，不引入 Redis、消息队列、微服务或复杂权限系统。

## 3. 核心决策

| 项目 | 决策 |
| --- | --- |
| 链 | Ethereum Sepolia，Chain ID `11155111` |
| 后端语言 | Go 1.23.5，与当前开发机保持一致 |
| Ethereum SDK | `github.com/ethereum/go-ethereum` 1.15.11 |
| 应用形态 | 单个 Go API 进程，内部运行扫描和提币 Worker |
| 数据库 | PostgreSQL 17+ |
| 缓存/队列 | 第一阶段不使用，依靠 PostgreSQL 事务、唯一约束和行锁 |
| RPC | 可配置 HTTP JSON-RPC，支持主备端点和退避重试 |
| 扫描方式 | 按高度轮询完整区块，不依赖 WebSocket |
| 地址标准 | BIP-39 + BIP-32/BIP-44，`m/44'/60'/0'/0/{index}` |
| 交易类型 | EIP-1559 Dynamic Fee Transaction |
| 金额单位 | 后端和数据库统一使用 Wei 整数；API 资产数值使用字符串 |
| 第一阶段出款地址 | 用户自己的托管充值地址 |
| 数据主键 | PostgreSQL `BIGINT GENERATED ALWAYS AS IDENTITY` 自增主键 |
| 业务时区 | `Asia/Shanghai`（UTC+8） |
| 前端形态 | SPA；框架在前端任务开始前最终确定 |

`go-ethereum` 1.15.11 的模块最低版本为 Go 1.23，可以直接使用当前开发机的 Go 1.23.5。更新的 `go-ethereum` 1.16.7 和 1.17.3 均要求 Go 1.24，因此第一阶段不采用。项目必须锁定 1.15.11，不能使用无版本约束的 `latest`。

## 4. 明确的阶段性取舍

### 4.1 为什么直接从用户充值地址提币

主流中心化交易所通常采用：为用户分配平台控制的充值地址，确认后给用户内部余额入账，再将链上资金归集到平台热钱包或冷钱包，最终由热钱包统一处理提币。

用户充值地址属于我们的托管钱包体系：地址对应的私钥由平台掌握，用户没有私钥。资产实际记录在区块链上的该地址中；数据库中的用户余额表示平台内部对用户的资产归属记录，并不是链上另一个钱包。

第一阶段没有自动归集。如果充值资金停留在用户地址，而提币却统一从热钱包发出，就会额外引入热钱包备付、归集和平台垫付逻辑，无法形成自洽闭环。

因此第一阶段采用“隔离托管子钱包”模式。这是为了尽快跑通充提的折中过渡方案，不是最终交易所架构：

- 每个用户有独立派生地址；
- 充值进入该地址；
- 提币也从该地址签名发出；
- 用户承担该地址交易产生的实际 Gas。

后续归集阶段再切换为交易所常见模式：用户充值地址归集到热钱包，所有用户提币统一由热钱包出款。届时本阶段的 `KeyProvider`、扫描器和交易状态机仍可复用。

### 4.2 充值识别边界

第一阶段只识别普通顶层 Ethereum 交易中 `to` 等于用户充值地址且 `value > 0` 的转账。

合约内部调用产生的 ETH 转账不会直接出现在区块交易的 `to/value` 中，需要 Trace API 或第三方索引服务，因此明确推迟。页面必须提示用户使用普通 ETH 转账充值。

### 4.3 为什么不用 WebSocket

公开 WebSocket 连接存在断线、漏事件和连接数限制。区块高度轮询虽然延迟略高，但容易通过持久化检查点保证不漏块，适合托管钱包第一阶段。

## 5. 总体架构

```text
Browser SPA
    |
    | REST/JSON
    v
Go Application
    +-- HTTP API
    +-- Wallet/KeyProvider
    +-- Balance Service
    +-- Deposit Scanner
    +-- Deposit Confirmer
    +-- Withdrawal Worker
    +-- Transaction Confirmer
    +-- EVM RPC Client
    |
    +----------> PostgreSQL
    |
    +----------> Sepolia RPC Provider(s)
                           |
                           +--> Sepolia network
                           +--> Block explorer
```

所有后台 Worker 与 API 可以运行在同一进程，但模块之间只能通过明确接口和数据库状态交互。这样既保持 MVP 简洁，也为后续拆分独立签名服务或扫描服务保留边界。

## 6. 仓库结构

```text
mini-custody/
├── backend/
│   ├── cmd/
│   │   ├── api/                 # API 和后台 Worker 入口
│   │   └── walletgen/           # 生成测试网助记词的离线工具
│   ├── internal/
│   │   ├── app/                 # 生命周期和依赖装配
│   │   ├── config/
│   │   ├── api/http/
│   │   ├── wallet/              # HD 派生和 KeyProvider
│   │   ├── chain/evm/           # Sepolia RPC 与交易实现
│   │   ├── scanner/
│   │   ├── deposit/
│   │   ├── withdrawal/
│   │   ├── ledger/
│   │   └── store/postgres/
│   ├── migrations/
│   ├── go.mod
│   └── go.sum
├── frontend/                    # Web SPA，技术栈后续确定
├── deploy/
│   ├── compose.yaml             # PostgreSQL、后端和前端
│   └── env.example
└── docs/
```

第一阶段不建立按链拆分的微服务。`chain/evm` 必须实现面向业务的窄接口，后续 Solana 和 Bitcoin 通过各自适配器接入，而不是在业务层大量判断链类型。

## 7. 模块接口

### 7.1 KeyProvider

```go
type KeyProvider interface {
    Address(ctx context.Context, path string) (common.Address, error)
    SignTx(ctx context.Context, path string, tx *types.Transaction, chainID *big.Int) (*types.Transaction, error)
}
```

业务层只持有派生路径，不保存私钥。第一阶段实现 `MnemonicKeyProvider`；未来可以增加 Vault、HSM 或 MPC 实现而不改变提币业务接口。

### 7.2 EVMClient

```go
type EVMClient interface {
    ChainID(ctx context.Context) (*big.Int, error)
    BlockNumber(ctx context.Context) (uint64, error)
    BlockByNumber(ctx context.Context, number *big.Int) (*types.Block, error)
    HeaderByNumber(ctx context.Context, number *big.Int) (*types.Header, error)
    BalanceAt(ctx context.Context, account common.Address, number *big.Int) (*big.Int, error)
    PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)
    SuggestGasTipCap(ctx context.Context) (*big.Int, error)
    EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)
    SendTransaction(ctx context.Context, tx *types.Transaction) error
    TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}
```

接口封装 `ethclient.Client`，使扫描、提币和测试不直接依赖具体 RPC SDK。

### 7.3 其他边界

- `AddressService`：用户与派生索引、路径、地址的稳定映射；
- `DepositService`：发现充值、更新确认数、执行入账；
- `BalanceService`：余额查询、资金占用、释放和最终结算；
- `WithdrawalService`：请求校验、幂等创建和状态查询；
- `WithdrawalWorker`：Nonce、Gas、签名和广播；
- `ChainHealthService`：RPC、网络高度和扫描高度状态。

## 8. 密钥和地址设计

### 8.1 根密钥来源

第一阶段使用“加密根密钥文件 + 独立解密密码”的本地密钥方案：

1. `walletgen init` 离线命令生成 256 位熵对应的 BIP-39 助记词；
2. 工具使用 `filippo.io/age` 的 scrypt 口令加密格式生成 `custody-root.age`；
3. 加密文件保存密文，可以备份，但不得提交到 Git；
4. 应用通过 `CUSTODY_KEYSTORE_FILE` 读取加密文件；
5. 应用通过 `CUSTODY_KEYSTORE_PASSWORD` 获取解密密码，密码不得写入加密文件或数据库；
6. 应用启动时在内存中解密助记词并生成 Seed；
7. 签名时按派生路径临时生成子私钥，使用完毕后立即丢弃引用；
8. 日志、错误、指标和 API 禁止输出密码、助记词、Seed 或派生私钥。

`walletgen verify` 必须能够使用密码解密文件并输出根指纹和测试网地址，但不得输出助记词。首次创建时助记词只显示一次，操作者需要离线备份；丢失加密文件和助记词将无法恢复资金。

`.env.example` 只提供变量名，绝不包含真实密码或密钥。仓库中的固定测试向量只能用于单元测试，且派生地址必须明确标记为不可用于真实资产。

### 8.2 派生规则

- 网络账户：`m/44'/60'/0'`
- 外部地址分支：`0`
- 预留索引 `0`：未来平台归集/热钱包地址
- 用户地址从索引 `1` 开始：`m/44'/60'/0'/0/{user_index}`

数据库保存 `derivation_index` 和完整 `derivation_path`。地址使用 EIP-55 checksum 格式展示，查询与唯一约束使用统一的 20 字节地址或规范化小写十六进制值。

### 8.3 依赖策略

- 使用 `go-ethereum` 进行地址、交易、签名器和 EIP-1559 处理；
- HD 派生候选使用 `go-ethereum-hdwallet` 的稳定版本；
- 根密钥文件使用 `filippo.io/age` v1.2.1 加密，该版本支持 Go 1.23.5；
- 实现前必须用 BIP-39/BIP-44 固定测试向量验证派生结果；
- 如果候选库与 `go-ethereum` 1.15.11 不兼容，则改用经过测试的独立 BIP-32/BIP-39 实现，不能降低测试要求。

第一阶段不自行发明助记词、派生算法或加密格式。

### 8.4 已知限制

本地加密文件和进程内签名不等同于生产级托管安全。解密密码仍会进入应用进程，Go 运行时也无法保证所有私钥内存被可靠清零。该实现只允许 Sepolia，并通过 `Chain ID == 11155111` 硬校验防止误连主网。

## 9. 数据模型

所有业务表使用 `BIGINT GENERATED ALWAYS AS IDENTITY` 自增主键，外键统一使用 `BIGINT`。业务唯一性通过额外唯一约束保证，不使用业务字段替代主键。

所有时间字段使用 PostgreSQL `TIMESTAMPTZ`。数据库会按时间点保存，但数据库 Session、Go 应用、日志和 API 统一使用 `Asia/Shanghai`（UTC+8）；JSON 时间使用带 `+08:00` 偏移的 RFC 3339 格式。链上 Unix 时间戳读取后也统一转换为 UTC+8 展示。

所有资产数值保存 Wei 整数，数据库使用 `NUMERIC(78,0)`，Go 使用 `big.Int`，JSON 使用十进制字符串。

### 9.1 `users`

| 字段 | 说明 |
| --- | --- |
| `id` | 用户 ID |
| `code` | 稳定演示用户编码，唯一 |
| `display_name` | 页面展示名称 |
| `created_at` | 创建时间 |

### 9.2 `wallet_addresses`

| 字段 | 说明 |
| --- | --- |
| `id` | 地址记录 ID |
| `user_id` | 所属用户 |
| `network` | 固定为 `ethereum-sepolia` |
| `address` | 规范化地址，唯一 |
| `derivation_index` | 用户派生索引，唯一 |
| `derivation_path` | 完整 BIP-44 路径 |
| `next_nonce` | 本系统下一候选 Nonce |
| `created_at` | 创建时间 |

同一用户在同一网络只能存在一个有效地址。

### 9.3 `asset_balances`

| 字段 | 说明 |
| --- | --- |
| `id` | 自增主键 |
| `user_id` | 用户 ID |
| `asset` | 第一阶段固定 `ETH` |
| `available_wei` | 可用余额 |
| `pending_deposit_wei` | 待确认充值 |
| `pending_withdrawal_wei` | 已占用提币资金 |
| `version` | 乐观并发版本号 |
| `updated_at` | 更新时间 |

唯一约束为 `(user_id, asset)`。余额更新必须在数据库事务中通过自增主键锁定该行。

### 9.4 `balance_entries`

| 字段 | 说明 |
| --- | --- |
| `id` | 流水 ID |
| `user_id` / `asset` | 用户资产 |
| `entry_type` | `DEPOSIT_PENDING`、`DEPOSIT_CREDIT`、`WITHDRAW_RESERVE`、`WITHDRAW_FINALIZE`、`WITHDRAW_RELEASE`、`FEE_ADJUST` |
| `amount_wei` | 有符号变化量 |
| `reference_type` / `reference_id` | 关联充值或提币 |
| `created_at` | 创建时间 |

流水只追加不修改。唯一约束 `(entry_type, reference_type, reference_id)` 防止重复记账。

### 9.5 `deposits`

| 字段 | 说明 |
| --- | --- |
| `id` | 充值 ID |
| `user_id` / `address_id` | 归属 |
| `network` / `asset` | 网络和资产 |
| `tx_hash` | 交易哈希 |
| `tx_index` | 交易在区块中的位置 |
| `block_number` / `block_hash` | 首次发现区块 |
| `amount_wei` | 充值金额 |
| `confirmations` | 最近确认数 |
| `status` | `DETECTED`、`CONFIRMING`、`CONFIRMED`、`CREDITED` |
| `created_at` / `updated_at` | 时间 |

唯一约束 `(network, tx_hash, address_id)`。

### 9.6 `withdrawals`

| 字段 | 说明 |
| --- | --- |
| `id` | 提币 ID |
| `idempotency_key` | 客户端幂等标识，与用户组成唯一约束 |
| `user_id` / `address_id` | 用户和出款地址 |
| `to_address` | 目标地址 |
| `amount_wei` | 收款金额 |
| `reserved_fee_wei` | 预留最大网络费 |
| `actual_fee_wei` | Receipt 得出的实际费用 |
| `nonce` | 已分配 Nonce |
| `raw_tx` | 已签名交易编码，用于相同字节重播 |
| `tx_hash` | 交易哈希 |
| `block_number` | 确认区块 |
| `confirmations` | 最近确认数 |
| `status` | 提币状态 |
| `error_code` / `error_message` | 已清洗错误信息 |
| `created_at` / `updated_at` | 时间 |

唯一约束为 `(user_id, idempotency_key)`。`raw_tx` 不包含私钥，但具有广播能力，数据库权限仍需限制。

### 9.7 `chain_checkpoints`

| 字段 | 说明 |
| --- | --- |
| `id` | 自增主键 |
| `network` / `scanner` | 网络和扫描器业务标识 |
| `last_scanned_block` | 最后完整处理的区块高度 |
| `last_scanned_hash` | 对应区块哈希 |
| `updated_at` | 更新时间 |

唯一约束为 `(network, scanner)`。

### 9.8 `worker_errors`

保存 Worker、阶段、关联对象、可公开错误码、清洗后的消息、首次和最近发生时间以及重试次数。禁止保存 RPC URL 中的密钥和任何私钥材料。

## 10. 充值扫描设计

### 10.1 初始化

1. 启动时请求 `eth_chainId`，必须为 `11155111`；
2. 加载所有受监控地址到内存集合；
3. 读取 `chain_checkpoints`；
4. 无检查点时使用配置 `SEPOLIA_SCAN_START_BLOCK`；
5. 起始高度不得默认使用创世区块，首次部署建议使用部署时最新高度减一个小回看窗口。

### 10.2 扫描循环

```text
读取网络最新高度
    -> 从 checkpoint + 1 开始
    -> 每次最多处理配置数量的区块
    -> 获取完整区块及交易
    -> 筛选 to 属于监控地址且 value > 0 的交易
    -> 在同一数据库事务中保存充值和推进 checkpoint
    -> 继续下一批
```

处理某个目标交易时，在同一事务内执行：

1. 使用唯一约束插入充值；
2. 仅在确实插入新充值时锁定用户余额行；
3. 增加 `pending_deposit_wei`；
4. 追加唯一的 `DEPOSIT_PENDING` 流水。

处理某个区块时，只有该区块所有目标交易都成功持久化后才能推进检查点。依靠充值和流水唯一约束实现重复扫描安全。

### 10.3 确认和入账

确认 Worker 周期性计算：

```text
confirmations = latest_block - deposit.block_number + 1
```

达到配置阈值前只更新充值的确认数；待确认余额已在首次发现充值时增加。达到阈值后：

1. 再次查询原区块 Header；
2. 验证 Header Hash 等于保存的 `block_hash`；
3. 在一个数据库事务内锁定充值和用户余额；
4. 将充值变更为 `CONFIRMED`；
5. 从待确认充值移除金额并增加可用余额；
6. 追加 `DEPOSIT_CREDIT` 流水；
7. 将充值变更为 `CREDITED`。

如果区块哈希不一致，第一阶段停止该充值入账、记录明确错误并暂停扫描器，等待人工处理。自动回滚和重放属于后续链重组阶段。

### 10.4 RPC 限流

- HTTP `429`、`502`、`503`、`504` 和网络超时视为可重试；
- 使用指数退避、随机抖动和最大等待时间；
- 不可重试 JSON-RPC 错误立即记录；
- 失败请求不得推进扫描检查点；
- 切换备用 RPC 前必须验证 Chain ID；
- 页面展示当前使用端点的匿名名称，不展示完整 URL 或 API Key。

## 11. 提币设计

### 11.1 API 创建提币

请求必须携带 `Idempotency-Key`：

1. 校验用户、目标地址和十进制 ETH 金额；
2. 将 ETH 转成 Wei，禁止浮点数；
3. 读取当前 EIP-1559 费用并估算 Gas；
4. 以 `gasLimit * maxFeePerGas` 作为最大预留费用；
5. 在数据库事务中锁定余额行；
6. 校验 `available >= amount + reserved_fee`；
7. 从可用余额移至待处理提币；
8. 创建 `CREATED` 提币和 `WITHDRAW_RESERVE` 流水；
9. 相同幂等标识直接返回原提币。

第一阶段只发送无 Data 的普通 ETH 转账，预期 Gas Limit 为 21,000，但仍通过 `eth_estimateGas` 校验，不把常量作为唯一依据。

Worker 在签名前重新计算费用。如果新的最大费用高于原预留，必须在锁定余额后补充占用差额；可用余额不足时，提币失败并释放原占用。签名前还必须查询出款地址的链上余额，链上余额不足时不得签名。

### 11.2 Nonce 分配

每个地址独立分配 Nonce：

1. Worker 在事务中锁定 `wallet_addresses` 行；
2. 查询 `eth_getTransactionCount(address, "pending")`；
3. 使用 `max(chain_pending_nonce, wallet_addresses.next_nonce)`；
4. 将所用 Nonce 写入提币；
5. 将 `next_nonce` 更新为 `nonce + 1`；
6. 同一事务将提币推进到 `SIGNING`。

单实例 Worker 仍必须使用数据库行锁，保证未来多实例或并发提币不会生成相同 Nonce。

### 11.3 构建和签名

使用 EIP-1559 Dynamic Fee Transaction：

- `chainID = 11155111`；
- `nonce` 使用已持久化值；
- `to` 使用校验后的目标地址；
- `value` 为用户请求金额；
- `gasTipCap` 来自 RPC 建议值并受配置上限限制；
- `gasFeeCap` 根据最新 Base Fee、Tip 和安全倍数计算；
- `gas` 来自估算结果；
- `data` 为空。

签名完成后，必须在广播前持久化 `raw_tx`、`tx_hash`、费用参数和 `SIGNED` 状态。

### 11.4 广播和崩溃恢复

Worker 始终广播数据库中已经保存的相同 `raw_tx`：

- 广播成功：进入 `BROADCASTED`；
- RPC 返回 already known：按成功处理；
- 广播超时：保留 `SIGNED`，先按哈希查询，再决定重播；
- 明确的签名或余额错误：进入 `FAILED`；
- 无法判断是否广播：不得释放余额或换 Nonce，持续查询原哈希。

这样可以处理“节点已接收交易，但应用在保存广播结果前崩溃”的窗口，而不会创建第二笔不同交易。

### 11.5 确认和费用结算

确认 Worker 通过 `eth_getTransactionReceipt` 跟踪交易：

1. 无 Receipt：保持 `BROADCASTED/CONFIRMING`；
2. Receipt `status == 0`：交易链上失败，仍结算实际 Gas，退回未转出的金额；
3. Receipt `status == 1`：等待配置确认数；
4. 达到阈值后计算 `actual_fee = gasUsed * effectiveGasPrice`；
5. 从待处理提币扣除 `amount + actual_fee`；
6. 将预留费用与实际费用差额退回可用余额；
7. 追加最终流水并进入 `COMPLETED`。

在交易是否已广播不明确时，不能把提币简单标记失败并释放余额。

## 12. 状态机

### 12.1 充值

```text
DETECTED -> CONFIRMING -> CONFIRMED -> CREDITED
```

数据库更新必须通过明确的允许迁移函数完成，禁止业务代码任意修改状态。

### 12.2 提币

对外状态沿用需求文档，内部增加 `BROADCASTING` 和 `BROADCAST_UNKNOWN` 以处理公开 RPC 的不确定结果：

```text
CREATED -> SIGNING -> SIGNED -> BROADCASTING -> BROADCASTED -> CONFIRMING -> COMPLETED
    |          |          |            |              |
    +----------+----------+------------+--------------+-> FAILED
                              |
                              +-> BROADCAST_UNKNOWN -> BROADCASTED
```

只有明确确认未广播且不可继续的情况才能进入 `FAILED` 并释放资金。

## 13. API 设计

统一前缀 `/api/v1`，响应资产数值均为十进制字符串。

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/system/chains/sepolia` | RPC、网络高度和扫描状态 |
| `GET` | `/users` | 获取预置用户 |
| `GET` | `/users/{id}/wallet` | 地址和余额 |
| `GET` | `/users/{id}/deposits` | 用户充值记录 |
| `GET` | `/users/{id}/withdrawals` | 用户提币记录 |
| `POST` | `/users/{id}/withdrawals` | 创建提币，要求 `Idempotency-Key` |
| `GET` | `/withdrawals/{id}` | 查询提币状态 |
| `GET` | `/transactions` | 全局交易列表 |
| `GET` | `/worker-errors` | 最近 Worker 错误 |

提币请求示例：

```json
{
  "to_address": "0x...",
  "amount_eth": "0.010000000000000000"
}
```

响应同时包含：

- `amount_wei` 和格式化后的 `amount_eth`；
- `reserved_fee_wei`；
- `status`；
- 已产生时的 `tx_hash` 和 Sepolia 浏览器 URL。

错误响应至少包含稳定 `code`、用户可读 `message` 和 `request_id`。不得原样返回底层 RPC URL、数据库错误或敏感配置。

## 14. Web 页面设计边界

第一阶段只要求 SPA，不要求 SSR：

- 用户选择器固定在页面顶部；
- Dashboard 展示 Sepolia RPC、网络高度、扫描高度和错误；
- Account 展示 EIP-55 地址以及三类余额；
- Deposit 展示地址、复制按钮、二维码和确认进度；
- Withdraw 展示金额、地址、预估最大费用和实时状态；
- Transactions 展示哈希、状态、区块、确认数和 Sepolia 浏览器链接。

前端不得直接连接 RPC、读取助记词或签名。所有链上能力通过 Go API 完成。

前端框架在进入前端实现任务前单独确定，候选为 React + TypeScript + Vite + Ant Design。框架选择不得改变本设计的 API 和状态模型。

## 15. 配置

建议环境变量：

```text
APP_ENV=development
HTTP_ADDR=:8080
APP_TIMEZONE=Asia/Shanghai
DATABASE_URL=postgres://...
CUSTODY_KEYSTORE_FILE=./secrets/custody-root.age
CUSTODY_KEYSTORE_PASSWORD=...
SEPOLIA_RPC_URL=...
SEPOLIA_RPC_FALLBACK_URL=...
SEPOLIA_SCAN_START_BLOCK=...
SEPOLIA_CONFIRMATIONS=...
SEPOLIA_SCAN_INTERVAL=...
SEPOLIA_SCAN_BATCH_SIZE=...
SEPOLIA_EXPLORER_BASE_URL=https://sepolia.etherscan.io
```

启动时执行：

- 配置完整性校验；
- 根密钥文件解密和根指纹校验；
- Chain ID 校验；
- 数据库迁移版本校验；
- 已保存地址与助记词派生结果一致性校验；
- 费用上限和确认数合理性校验。

任一安全校验失败时，链上 Worker 不得启动。

## 16. 可观测性

第一阶段提供结构化日志和健康接口：

- 每个请求带 `request_id`；
- Worker 日志带 `network`、`worker`、`block`、`deposit_id` 或 `withdrawal_id`；
- RPC URL 只记录端点别名；
- 健康接口区分应用存活、数据库可用和 Sepolia RPC 可用；
- 页面展示最近错误和扫描延迟。

核心指标预留：

- 最新网络高度与扫描高度差；
- 每批扫描耗时和 RPC 错误数；
- 各状态充值/提币数量；
- 最老待确认交易时长；
- RPC 限流和端点切换次数。

Prometheus 接入可以在第一阶段后半段完成，但指标命名和模块边界从开始保留。

## 17. 测试方案

### 17.1 单元测试

- 固定助记词和路径得到固定地址；
- ETH/Wei 精确转换和边界值；
- 状态迁移是否合法；
- 确认数计算；
- Gas 最大预留和实际费用结算；
- Nonce 取最大值逻辑；
- RPC 错误分类和退避；
- 敏感信息清洗。

### 17.2 数据库集成测试

- 充值唯一约束和重复扫描；
- 同一充值只入账一次；
- 并发提币余额行锁；
- 幂等标识只创建一笔提币；
- Nonce 并发分配；
- 进程中断后从各状态恢复。

数据库测试使用临时 PostgreSQL，不需要本地区块链。

### 17.3 RPC 契约测试

使用受控 Mock JSON-RPC 响应验证：

- 区块和交易解码；
- Receipt 状态和费用计算；
- `429`、超时、already known 和 transaction not found；
- Chain ID 不匹配；
- RPC 返回高度暂时不一致。

Mock RPC 只用于自动测试确定性，不替代真实充提环境。

### 17.4 Sepolia 端到端测试

通过显式环境开关单独运行，不加入每次普通单元测试：

- 使用专用测试助记词和少量 Sepolia ETH；
- 从外部测试地址向托管地址充值；
- 等待扫描、确认和入账；
- 从托管地址提币到另一个测试地址；
- 验证 Receipt、实际费用、余额和浏览器链接；
- 在提币确认前重启应用，验证恢复。

测试不得自动请求水龙头，也不得因水龙头不可用而判定业务代码失败。

## 18. 实施顺序

1. 建立 Go 1.23.5 项目骨架和 PostgreSQL；
2. 实现配置、迁移和预置用户；
3. 实现助记词工具、地址派生和 KeyProvider 测试向量；
4. 实现 Sepolia RPC 封装和网络校验；
5. 实现区块扫描、充值记录和确认入账；
6. 实现余额占用和提币 API；
7. 实现 Nonce、EIP-1559 签名、广播和确认；
8. 实现 Web 页面；
9. 完成重启恢复、RPC 限流和 Sepolia 端到端验收。

具体任务和依赖关系在 `tasks.md` 中拆分。

## 19. 风险与后续演进

| 风险/限制 | 第一阶段处理 | 后续演进 |
| --- | --- | --- |
| 根密钥在应用进程内解密 | age 加密文件、密码分离、仅 Sepolia、禁止输出 | Vault/HSM/MPC、独立签名服务 |
| 用户地址直接提币 | 保持资金闭环 | 自动归集和热钱包统一出款 |
| 不追踪内部 ETH 转账 | 页面明确提示普通转账 | Trace API 或链上索引服务 |
| RPC 限流和不稳定 | 退避、主备端点、持久化检查点 | 专用 RPC、节点池和熔断 |
| 未自动处理链重组 | 入账前校验区块哈希，不一致则停机报警 | 自动回滚、重放和对账 |
| Sepolia 测试币获取不稳定 | 人工准备测试资金 | 测试资金池和余额告警 |
| 单体 Worker | 数据库锁保证正确性 | 按链拆分 Worker、多实例租约 |

## 20. 参考资料

- Ethereum 网络与 Sepolia：https://ethereum.org/developers/docs/networks/
- Go Ethereum：https://github.com/ethereum/go-ethereum
- Go Ethereum Releases：https://github.com/ethereum/go-ethereum/releases
- Go 发布历史：https://go.dev/doc/devel/release
- Ethereum HD Wallet for Go：https://github.com/miguelmota/go-ethereum-hdwallet
- age 加密格式和 Go 库：https://github.com/FiloSottile/age
