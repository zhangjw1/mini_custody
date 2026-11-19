# 后端问题修复方案

> 来源：2026-11-19 后端代码审查。每个批次都可以单独交给一个 agent 执行，批次内的任务按顺序完成。
> 路径相对 `backend/`。行号以审查当时的代码为准，修改前请重新定位。

## 0. 执行顺序与并行约束

| 批次 | 内容 | 主要改动文件 | 迁移号 | 依赖 |
|------|------|--------------|--------|------|
| A | BTC 资金安全 | `internal/store/postgres/bitcoin*.go`、`internal/btc/*` | 000004 | 无 |
| B | EVM nonce 空洞与卡单 | `internal/withdrawal`、`internal/token/*`、`balance_transaction.go`、`token_*_transaction.go` | 无（B2 若需要，用 000006） | 无 |
| C | 链重组处理 | `internal/deposit/scanner.go`、`internal/token/deposit/scanner.go`、`internal/btc/scanner*.go`、`app/app.go` 组件退出逻辑 | 无 | A 完成后再动 BTC 扫描部分 |
| D | API 鉴权与 HTTP 加固 | `internal/app/*handler*.go`、`app.go` 路由和 server、`internal/config` | 无 | 无 |
| E | 性能（索引、N+1、连接池） | `migrations`、`repository.go`、`internal_transfer_transaction.go`、`db.go`、各扫描器 | 000005 | A、B、C 合并后开始 |
| F | 重构（去重、拆文件、魔数） | 全局 | 无 | 其他批次全部合并后开始 |

A、B、D 可以同时进行。C 的 EVM 部分可以和 A 并行，BTC 部分要等 A 合并后再做。E、F 修改面大，放到最后，避免冲突。

---

## 1. 通用提示词前缀（每个 agent 都要带上）

```text
你在 Go 1.23 + pgx/v5 的加密货币托管项目 mini_custody 中工作，后端目录为 backend/。
开始前先阅读 docs/development/coding-conventions.md 和本文件 docs/development/backend-remediation-plan.md 中分配给你的批次。

硬性约束：
1. 不要修改已有的迁移文件 000001–000003，新增迁移使用本文件指定的版本号，并同时提供 up/down；如果 migrations/verify.go 有结构检查，要同步更新。
2. 函数注释、日志、返回的错误都使用中文（项目约定，internal/style 的 AST 测试会检查）；底层错误用 %w 包装。
3. 状态值、锁标识、时间间隔等不要写裸字符串和魔数，在对应包里定义常量。
4. 所有资金相关的 UPDATE 都要带状态前置条件，并校验 RowsAffected 或 RETURNING 的结果，不允许重复结算。
5. 余额变动必须在同一事务里写 balance_entries。
6. 测试使用真实 PostgreSQL（TEST_DATABASE_URL，make integration-test），不要 mock 数据库；链上 RPC 可以用现有的 fake/接口替身。
7. 只做本批次范围内的改动，不要顺手重构其他代码。
8. 完成后运行 go vet ./...、go test ./...、make integration-test，在回复里贴出结果，并逐条列出每个任务的改动和对应测试。
```

---

## 2. 批次 A：BTC 资金安全（最高优先级）

### A1 补 `btc_withdrawals.raw_tx` 列
- **问题**：`migrations/000003_bitcoin.up.sql:80` 只有 `raw_tx_hash`，但 `bitcoin_withdrawal_worker.go:19,109` 读写 `raw_tx`，BTC 提币 worker 一运行就报 column does not exist。
- **方案**：新增 `000004_btc_fixes.up.sql`，执行 `ALTER TABLE btc_withdrawals ADD COLUMN raw_tx BYTEA`；down 对应删除该列。A 批次的其他 DDL 也放进这个迁移。
- **验收**：迁移 up/down/up 可以往返；集成测试能完整跑通 `ListProcessableBTCWithdrawals` 和 `SaveBTCWithdrawalSigned`。

### A2 签名时落库 txid，修复广播后崩溃导致的重复出款
- **问题**：
  - `internal/btc/withdrawal_worker.go` 的 `broadcast()` 在 `SIGNED` 状态下先调 `TestMempoolAccept`，被拒绝就走 `FailBTCWithdrawal`。
  - `FailBTCWithdrawal`（`bitcoin_withdrawal_worker.go:156`）允许 `SIGNED → FAILED`，会退款并把 UTXO 置回 `UNSPENT`。
  - 如果广播成功后、`MarkBTCWithdrawalBroadcasted` 执行前进程崩溃，重启后 mempool 会拒绝这笔交易，结果是链上已经出款，用户又拿回了余额。
  - 归集同理：`sweep_worker.go:103-110`、`bitcoin.go:83` 的 `FailBTCSweep`。
- **方案**：
  1. `SaveBTCWithdrawalSigned` 和归集签名时，用 raw tx 计算出 txid，同时写入 `txid` 列。
  2. 广播前，以及 `TestMempoolAccept` 被拒绝后，先用 txid 调 `getrawtransaction` 或 `gettxout` 查交易是否已在链上或 mempool；查到就直接转 `BROADCASTED`。
  3. `FailBTCWithdrawal` 和 `FailBTCSweep` 只允许 `CREATED` 和 `INPUTS_LOCKED` 状态失败。`SIGNED` 状态下被 mempool 拒绝、且链上查不到时，转入人工处理状态，并写入 worker_errors，不自动退款。如果需要新状态，在 000004 里修改 CHECK 约束。
  4. `BROADCAST_UNKNOWN` 状态同样先按 txid 查链。`withdrawal_worker.go:144` 不要再吞掉 `MarkBTCWithdrawalBroadcastUnknown` 的错误。
- **验收**：集成测试模拟"已广播但状态仍为 SIGNED"，重新处理后状态变为 `BROADCASTED`，余额和 UTXO 都不回滚。

### A3 `CompleteBTCWithdrawal` 加状态前置条件，找零入库，补流水
- **问题**：`bitcoin_withdrawal_worker.go:126-146`：
  - UPDATE 不检查当前状态，重复调用会把 `pending_withdrawal_wei` 扣两次，FAILED 的单子也能被改成 COMPLETED；
  - 找零输出没有插入 `btc_utxos`；
  - 完成和失败都不写 `balance_entries`。
- **方案**：
  - UPDATE 加上 `AND status IN ('BROADCASTED','CONFIRMING')`，并用 RETURNING 判断是否命中；已经是 COMPLETED 的直接幂等返回。
  - `change_sats > 0` 时，把找零输出（vout 按构建顺序确定，写成常量或在构建时记录下来）以 `UNSPENT` 插入 `btc_utxos`，地址为 `PLATFORM_CHANGE`，使用 `ON CONFLICT DO NOTHING`。
  - 完成时写 `WITHDRAW_COMPLETE` 流水，失败时写 `WITHDRAW_RELEASE` 流水，entry_type 与 ETH 保持一致。
- **验收**：并发调用两次 `CompleteBTCWithdrawal`，余额只扣一次、流水只有一条；找零 UTXO 可以被下一笔提币选中。

### A4 充值 UTXO 锁给归集任务，修复归集与提币重复使用同一 UTXO
- **问题**：
  - `CreditBTCDeposit`（`bitcoin.go:448-458`）把充值 UTXO 置为 `UNSPENT` 并创建归集任务，但没有锁定这个 UTXO。
  - `LockWithdrawalInputs`（`bitcoin_withdrawal_worker.go:55`）会选中所有 `UNSPENT` 的 UTXO，包括正在归集的。
  - `CompleteBTCSweep`（`bitcoin.go:109`）只更新 `status='LOCKED'` 的行，永远匹配不上，所以归集完成后输入仍显示 `UNSPENT`。
  - `CreateBTCSweepForDeposit` 里已经有锁定逻辑，但没有任何地方调用。
- **方案**：
  - 入账时在同一事务里把 UTXO 置为 `LOCKED`，`locked_by` 设为归集锁标识，可以复用或合并 `CreateBTCSweepForDeposit`。
  - 归集锁不设过期时间，或者确保 `ReleaseExpiredBTCUTXOLocks` 不会释放归集锁。
  - 提币选币只选平台地址（`TREASURY` 和 `PLATFORM_CHANGE`）的 UTXO，SQL 中 JOIN `btc_addresses` 并按 purpose 过滤。
  - 删除确认无用的死代码：`LockBTCUTXOs` 及其测试要么接入使用，要么删除，二选一并说明理由。
- **验收**：集成测试中，充值入账后发起提币，不能选中充值 UTXO；归集完成后，原 UTXO 为 `SPENT`，归集输出为 `UNSPENT`。

### A5 `LockWithdrawalInputs` 状态推进与重试复用
- **问题**：
  - 锁定 UTXO 后提币状态仍为 `CREATED`。如果签名前崩溃，下一轮会再锁一批新的 UTXO，完成时所有 `locked_by=withdrawal-<id>` 的 UTXO 都会被标成 `SPENT`。
  - 这个函数会对全部 `UNSPENT` 行加 `FOR UPDATE`。
  - 第 73、81 行用 `string(rune(vout))` 拼 map key，有冲突风险。
- **方案**：
  - 锁定后把状态推进到 `INPUTS_LOCKED`，并把选中的输入写入 `selected_inputs_json`。
  - worker 处理 `INPUTS_LOCKED` 时复用已锁的输入，不重新选币。
  - map key 改用 `u.ID`。
  - 锁标识 `"withdrawal-"+id` 抽成一个函数，统一 store 和 worker 两处的用法。
- **验收**：模拟"锁定后、签名前崩溃"，重试时使用同一批 UTXO，没有多锁。

### A6 选币与构建的手续费估算统一
- **问题**：
  - `internal/btc/select.go:24` 按 1 个输出估算手续费，`withdrawal_builder.go:21` 按 2 个输出估算，选币通过后构建经常报余额不足。
  - 选币是不剪枝的指数级穷举，最多 12 个输入。
- **方案**：
  - 抽出同一个 `EstimateVSize(inputs, outputs)` 函数，选币按"目标输出 + 找零"计算；找零低于 dust 阈值时并入手续费。
  - 选币改为"按金额降序累加，超过目标后停止"，或者使用带上界剪枝的分支定界算法，并限制最大候选数量。
  - 提币金额要校验 dust 下限。
  - 网络参数不要写死 `SigNetParams`（`withdrawal_service.go:33`、`bitcoin_withdrawal_worker.go:47`），改为从配置读取。
- **验收**：单元测试覆盖"刚好够、差 1 sat、需要找零、找零为 dust"四种情况；选币和构建的结果一致。

### 批次 A 提示词

```text
<通用提示词前缀>

你负责批次 A（BTC 资金安全），按 A1 → A6 顺序完成，迁移版本号使用 000004。
这些都是资金安全问题，每个任务必须带一个集成测试来证明问题被修复：先写出能复现问题的失败测试，再修改代码让它通过。
A2 中"SIGNED 被拒绝且链上查不到"的情况不要自动退款，转入人工处理状态即可。
完成后单独列出：新增的状态和常量、迁移内容、每个任务对应的测试名。
```

---

## 3. 批次 B：EVM nonce 空洞与卡单

### B1 分配 nonce 后不允许释放或无限重试
- **问题**：
  - ETH 提币：`internal/withdrawal/worker.go:155-179` 的 `sign()` 运行时 nonce 已经分配，这时如果出现 `FEE_RESERVE_INSUFFICIENT` 或 `CHAIN_BALANCE_INSUFFICIENT`，会调用 `ReleaseWithdrawal`。`balance_transaction.go:648` 允许释放 `SIGNING` 状态，但 `next_nonce` 不会回退，留下的 nonce 空洞会让这个地址之后的交易永久卡住。
  - Token 提币（`token/withdrawal/worker.go:166-168`）、归集（`token/sweep/worker.go:240-257`）、补气（`token/gasstation/worker.go:277-279`）在 SIGNING 阶段库存或 gas 不足时，会带着已分配的 nonce 无限重试。
- **方案**：
  1. 把费用、链上余额、库存、gas 等所有可能失败的校验都移到分配 nonce 的事务之前，或者放进同一个事务里。分配 nonce 之后只做签名和持久化。
  2. `ReleaseWithdrawal` 拒绝已分配 nonce 的单子。如果实在需要释放，在同一个行锁事务里判断 `next_nonce == nonce+1` 才回退，否则返回 `ErrUnsafeRelease`。
  3. 已分配 nonce 后如果连续失败超过阈值，转入人工处理状态并写入 worker_errors，不要每轮都写一条错误。
- **验收**：集成测试中，链上余额不足的提币不会占用 nonce；人为制造 nonce 已分配的单子，调用 release 会被拒绝。

### B2 卡单检测、重播与 gas 加价替换（先设计后实现）
- **问题**：
  - 4 个 worker 在 `BROADCASTED` 状态下，查 Receipt 返回 NotFound 就直接 `return nil`（`withdrawal/worker.go:277-280`、`token/withdrawal/worker.go:257-260`、`token/sweep/worker.go:353-356`、`token/gasstation/worker.go:341-344`）。
  - `maxFee` 固定为 `2×baseFee + tip`（`withdrawal/service.go:210`）。
- **方案**：
  - 记录 `broadcasted_at`，超过阈值后重播原 raw_tx。
  - 再次超时后，用同一 nonce 重新签名，tip 和 feeCap 各上调至少 10%（有上限，从配置读取）。
  - 新建表或 JSON 列保存候选 tx_hash，确认时任一 hash 命中即完成，并按实际交易结算手续费。
  - 遇到 "nonce too low" 且链上 nonce 已越过、但没有任何候选交易被确认时，转人工处理。
- **要求**：先在本文件末尾追加设计小节（状态机、表结构、阈值配置），等用户确认后再写代码。

### B3 RPC 端点切换
- **问题**：`internal/chain/evm/client.go:426-430` 遇到不可重试的错误直接 return，不会尝试下一个端点，connection refused 和 HTTP 5xx 都属于这一类。另外 `markFailure` 在判断是否可重试之前就被调用了，NotFound 和 revert 这类正常结果也会把健康状态标为 DEGRADED。
- **方案**：
  - 把错误分为业务错误（NotFound、revert、nonce 相关、余额不足）和端点错误（网络错误、5xx、超时、限流）。
  - 端点错误切换到下一个端点；业务错误直接返回，并且不调用 `markFailure`。
- **验收**：单元测试中，主端点返回 connection refused 时会切到备用端点；NotFound 不影响健康状态。

### 批次 B 提示词

```text
<通用提示词前缀>

你负责批次 B，按 B1 → B3 → B2 的顺序进行。
B1、B3 直接实现并补测试。
B2 只做设计：在 docs/development/backend-remediation-plan.md 末尾追加"B2 设计"小节，写清状态机、表结构变更（迁移号 000006）、配置项和恢复流程，然后停下来等用户确认，不要写 B2 的代码。
B1 必须覆盖 ETH 提币、Token 提币、Token 归集、Gas Station 四条链路。
```

---

## 4. 批次 C：链重组处理

### C1 EVM 扫块校验父哈希
- **问题**：
  - `internal/deposit/scanner.go` 和 `internal/token/deposit/scanner.go` 不校验 `ParentHash` 是否等于检查点的 `LastScannedHash`，重组只能在入账时才被发现。
  - 发现后 Run 返回错误，`app/app.go:393-397` 收到第一个组件退出就会 cancel 所有组件。
  - 同一笔交易被重新打包进新区块时，`ON CONFLICT DO NOTHING` 保留了旧的 block_hash，重启后再次比对不一致，进入崩溃循环。
  - `balance_transaction.go:186-189`：同一个 tx_hash 出现在不同区块时，直接返回 `ErrIdempotencyConflict`。
- **方案**：
  1. 扫块时校验父哈希，不一致就把检查点回退 N 个块（N 从配置读取，默认值等于确认数）。
  2. 回退区间内处于 CONFIRMING 的充值：如果交易在新链上还在，更新 block_hash 和 block_number；如果已经不在，标记为 ORPHANED（如需新增状态，使用迁移 000007），并冲正 pending 余额，同时写冲正流水。
  3. 已经 CREDITED 的充值发生重组时，不自动扣款，写 worker_errors 并告警。
  4. 扫描上限改为 `latest - safeDepth`。
- **验收**：集成测试模拟"检查点区块被替换"，扫描器会回退并重新扫描，不会退出 Run。

### C2 单条异常不能拖垮整个应用
- **问题**：
  - 扫描器和 worker 遇到单条数据异常时，Run 会返回错误，导致整个 app 退出。
  - BTC 侧：`internal/btc/scanner_worker.go:141-143` 遇到孤块充值哈希不一致时直接 return error，阻塞后面全部入账。
- **方案**：
  - 单条异常记入 worker_errors 后跳过，继续处理后续数据。
  - 只有不可恢复的错误（例如数据库不可用）才让 Run 返回。
  - BTC 孤块充值走已有的 `RewindBTCCheckpoint` 逻辑，并改为写冲正流水，而不是 DELETE 流水。
- **验收**：一条异常充值不会影响同一批次中其他充值入账。

### 批次 C 提示词

```text
<通用提示词前缀>

你负责批次 C（链重组处理）。先完成 C1 的 EVM 部分（ETH 和 Token 扫描器）以及 C2 中的 EVM 部分。
BTC 扫描器的改动要等批次 A 合并后再做：开始 BTC 部分前，先用 git log 确认批次 A 已合并，没有合并就停下来告诉用户。
需要新增充值状态时，在 docs/development/backend-remediation-plan.md 的批次 C 下写明，迁移号使用 000007。
```

---

## 5. 批次 D：API 鉴权与 HTTP 加固

### D1 鉴权
- **问题**：`app/app.go:280-310` 的所有路由都没有鉴权。任何人都能对任意 `user_id` 发起提币，也能读取 `worker-errors`、平台钱包状态等接口。
- **方案**：
  - 增加鉴权中间件，至少实现 API Key（从配置读取，不要硬编码）。
  - 区分管理接口（`/api/v1/system/*`、`/worker-errors`、`/sweeps`、`/internal-transfers`、`/transactions`）和用户接口：用户接口要校验调用方是否有权限操作对应的 `user_id`。
  - 如果项目定位是 demo，至少要求管理接口和所有 POST 接口带 API Key。
  - `healthz` 不需要鉴权。
- 先在本节写出采用的方案，再实现。

### D2 HTTP 加固
- **问题**：
  - `app.go:311-316` 只设了 `ReadHeaderTimeout`。
  - `bitcoin_withdrawal_handler.go:46-49` 把所有错误都按 400 返回，并原样输出 `err.Error()`，会暴露 PG 约束名和 SQLSTATE。
  - BTC handler 没有 `MaxBytesReader`，也不拒绝多余的 JSON。
- **方案**：
  - 补上 `ReadTimeout`、`WriteTimeout`、`IdleTimeout`，从配置读取。
  - 统一错误映射：余额不足和幂等冲突返回 409，参数错误返回 400，未找到返回 404；其他错误返回 500 和通用提示，详细信息只写日志。
  - 三套 handler（ETH、Token、BTC）共用一个请求解码函数、一个错误写出函数和一个 `parseUserID`。
  - 删除 `bitcoin_api_handler.go:39` 里永远不成立的 `a.store==nil` 判断。
  - 拆分健康检查：`/livez` 表示存活，`/readyz` 表示就绪并包含 BTC 状态；LOW_BALANCE 属于告警，不应返回 503。
- **验收**：handler 测试覆盖各类错误码；任何响应中都不出现 SQLSTATE 和约束名。

### D3 优雅退出
- **问题**：`app.go:399-401,420-424` 出错时提前 return，不等 worker 结束，defer 就把连接池关了。
- **方案**：cancel 之后，等所有组件的 goroutine 退出（可设超时）再关闭连接池。各 worker 的 `RunOnce` 循环里检查 `ctx.Err()`，退出时不再用已取消的 ctx 写错误。
- 顺便修正 `app.go:392` 的注释：这里实际是在等第一个组件退出。

### 批次 D 提示词

```text
<通用提示词前缀>

你负责批次 D（API 鉴权与 HTTP 加固），按 D1 → D2 → D3 完成。
D1 先在 docs/development/backend-remediation-plan.md 的 D1 小节写下采用的鉴权方案（一段话即可），然后直接实现，不需要等确认。
修改接口行为（状态码、新增 Header）时，同步更新 docs/api.md；前端如果依赖这些状态码，列出受影响的位置，但不要修改前端代码。
```

---

## 6. 批次 E：性能

- **E1 索引**（迁移 000005）：
  - 新增 `deposits(user_id, id DESC)`、`btc_deposits(user_id, id DESC)`、`btc_utxos(network, status, block_height, id)`、`btc_utxos(locked_by) WHERE locked_by IS NOT NULL`。
  - `balance_entries` 和 `token_deposits` 的查询按 id 排序，但索引建在 created_at 上，二者要对齐。
  - 每条索引都要先用 `EXPLAIN` 对比前后执行计划，并把结果写进 PR 描述。
- **E2 N+1**：
  - `ListGasStationSweeps`（`internal_transfer_transaction.go:76-82`）改为一条 JOIN 查询。
  - `RecordBTCDepositsAndCheckpoint` 里查 `btcAssetID` 的语句移到循环外（`bitcoin.go:399`）。
  - `recordDepositInTx` 去掉先插入 DETECTED 再改成 CONFIRMING 的多余步骤，末尾的回查 SELECT 改用 RETURNING。
  - EVM 扫描器的 `IsInternalTransferTx` 改为按块批量查询；`HeaderByNumber` 按块高缓存。
- **E3 RPC 批量**：
  - 扫块改为 `BatchCallContext` 批量拉块，按批提交检查点。
  - worker 每轮只取一次 `BlockNumber`、`SuggestGasTipCap` 和平台钱包。
  - 地址表改为增量加载。
- **E4 交易列表分页**：
  - `ListTransactionsFilteredPage`（`repository.go:247-282`）改为在每张子表内先按条件过滤，并各取 `LIMIT offset+limit` 条，再在外层合并；或者改成游标分页。
  - 结果里补上 BTC 的记录。
- **E5 连接池**：`db.go` 的 MaxConns、MinConns、MaxConnLifetime、MaxConnIdleTime 从配置读取；`NewWithConfig` 和 `Ping` 的错误要用 `%w` 包装，不能吞掉。
- **E6**：`amount/wei.go:22` 的正则改为包级变量预编译；`wallet/bitcoin.go:90` 缓存 master key。

### 批次 E 提示词

```text
<通用提示词前缀>

你负责批次 E（性能），开始前用 git log 确认批次 A、B、C 已经合并。迁移号使用 000005。
每项优化都要给出前后对比：SQL 优化给 EXPLAIN，RPC 优化给每处理 N 个块的调用次数。不要改变任何业务语义；现有测试必须全部通过。
```

---

## 7. 批次 F：重构（最后做）

- 抽取公共函数：
  - store 层：`lockRowForUpdate`、`insertBalanceEntry`、`adjustBalance(delta)`；
  - 4 个 EVM worker：`FeeEstimator`、`Broadcaster`、`Confirmer`，以及通用的轮询循环。
- 拆文件：
  - `balance_transaction.go`（865 行）按充值和提币拆成两个文件；
  - `app.go` 的 `New`（约 250 行）按链拆成 `wireEVM`、`wireERC20`、`wireBTC`。
- 常量化：
  - UTXO 状态、BTC 提币状态、entry_type；
  - worker 批量大小（100、200）、`10 minutes`、RPC 错误码 -32002/-32005、baseFee 倍数等。
  - `allowedWithdrawalTransition` 改为包级 map，不要每次调用都新建。
- 补充 .gitattributes，统一换行符。

### 批次 F 提示词

```text
<通用提示词前缀>

你负责批次 F（重构），开始前用 git log 确认 A–E 都已合并。
只做等价重构，不改变任何行为。每完成一组抽取就运行一次全部测试。
每个重构单独提交，方便回滚。
```
