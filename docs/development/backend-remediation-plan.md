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

### 批次 C 状态与流水约定（迁移 `000007_reorg_orphaned`）
- 新增充值状态：`ORPHANED`，同时扩展 `deposits.status`、`token_deposits.status`、`btc_deposits.status` 约束；将上述三张表的幂等唯一约束替换为 `WHERE status <> 'ORPHANED'` 部分唯一索引（以及 `btc_utxos` 的 `WHERE status <> 'UNKNOWN'`），保证孤块冲正保留不可变记录且同一交易随后重新上链仍可入账。
- 新增余额流水类型：`DEPOSIT_REVERSAL`（只追加负向冲正流水，禁止 `DELETE FROM balance_entries`）。

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

---

## 8. B2 设计：EVM 卡单检测、重播与 Gas 加价替换

> 本设计覆盖 4 条 EVM 出款/运维链路：`withdrawals`（ETH 提币）、`token_withdrawals`（ERC-20 提币）、`token_sweeps`（Token 归集）、`internal_transfers`（Gas Station 补气）。

### 8.1 目标与约束
- 同一个 `nonce` 允许生成多笔替换交易（RBF / EIP-1559 Replacement），所有候选 `tx_hash` 都必须持久化在数据库，确认时只要任一候选交易上链打包即完成结算并按**实际成交的那笔交易**结算 `actual_fee_wei`。
- 严禁更换 `nonce` 重发（会造成双花或卡单永久锁死），严禁在未确认所有候选 `tx_hash` 均未上链前释放余额。
- 当链上 `PendingNonce / LatestNonce` 已越过本单 `nonce`，且所有候选 `tx_hash` 均未拿到成功 Receipt 时，必须转入 `MANUAL_REVIEW` 人工核查状态。

### 8.2 状态机设计

```text
SIGNED ──(首次广播)──> BROADCASTED ──(拿到 Receipt)──> CONFIRMING ──> COMPLETED / FAILED
  │                        │
  │ (超时无 Receipt)       │ (超过阈值 B2_REBROADCAST_AFTER)
  ▼                        ▼
BROADCAST_UNKNOWN ──> 重播原 raw_tx（保持相同 tx_hash）
                           │
                           │ (再次超过阈值 B2_BUMP_AFTER，且未达最大替换次数 B2_MAX_BUMPS)
                           ▼
                      以同一 nonce 重新签名：
                      tip = max(prevTip * 1.15, suggestTip)
                      feeCap = max(prevFeeCap * 1.15, 2*baseFee + tip)
                      （不超过 B2_MAX_FEE_CAP_WEI / 预留额度限制）
                      追加写入候选 tx_hash 表，更新主表最新 raw_tx/tx_hash -> 继续 BROADCASTED 轮询
                           │
                           │ (若 chain LatestNonce > item.Nonce 且所有候选 tx_hash Receipt 均 NotFound)
                           ▼
                      MANUAL_REVIEW（写 worker_errors，停止自动重发与释放，等待人工处理）
```

### 8.3 数据库表结构变更（迁移号 `000006_evm_tx_replacement`，连续递增）
1. 为 4 张业务表增加列：
   - `broadcasted_at TIMESTAMPTZ`（首次广播时间戳，重播与加价计时起点）
   - `last_broadcast_at TIMESTAMPTZ`（最近一次广播/重播时间）
   - `bump_count INTEGER NOT NULL DEFAULT 0`（已执行加价替换次数）
   - 将 4 张表的 `status` CHECK 约束补充 `'MANUAL_REVIEW'`。
2. 新增候选交易历史表 `evm_tx_attempts`（统一记录 4 类业务发送过的全部版本签名交易，避免原 `tx_hash` 被覆盖后丢失）：
   - `id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY`
   - `business_type TEXT NOT NULL`（`'WITHDRAWAL' | 'TOKEN_WITHDRAWAL' | 'TOKEN_SWEEP' | 'INTERNAL_TRANSFER'`）
   - `business_id BIGINT NOT NULL`
   - `from_address TEXT NOT NULL`
   - `nonce NUMERIC(20, 0) NOT NULL`
   - `attempt_no INTEGER NOT NULL`
   - `tx_hash TEXT NOT NULL`
   - `raw_tx BYTEA NOT NULL`
   - `max_fee_per_gas_wei NUMERIC(78, 0) NOT NULL`
   - `max_priority_fee_per_gas_wei NUMERIC(78, 0) NOT NULL`
   - `gas_limit BIGINT NOT NULL`
   - `created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP`
   - 约束：`UNIQUE (business_type, business_id, attempt_no)`、`UNIQUE (tx_hash)`。
   - 同时为所有新表和新列添加中文注释，并把 `evm_tx_attempts` 加入 `migrations/verify.go`。

### 8.4 配置项设计（`internal/config/config.go`）
- `EVM_REBROADCAST_AFTER`（默认 `60s`）：`BROADCASTED` 无 Receipt 后触发原 `raw_tx` 重播的阈值。
- `EVM_BUMP_AFTER`（默认 `180s`）：重播仍卡在 mempool 后触发加价替换的阈值。
- `EVM_BUMP_PERCENT`（默认 `15`，校验 `>= 10` 满足节点 10% 最小替换门槛）。
- `EVM_MAX_BUMPS`（默认 `3`）：单笔交易最大加价替换次数。
- `EVM_MAX_FEE_CAP_GWEI`（默认 `100`）：加价后的硬顶上限，防止 Gas 异常飙升耗尽热钱包资金。

### 8.5 费用与恢复流程要点
1. **ETH 提币资金约束**：由于用户预留总扣款已在 `ReserveWithdrawal` 锁定为 `amount_wei + reserved_fee_wei`，加价后的 `gas_limit * new_max_fee_per_gas` 不得超过 `item.ReservedFeeWei`（除非在加价前先对 `asset_balances` 原子增加预留费，若用户可用余额不足则不再加价，仅继续重播或转人工）。
2. **多候选 Hash 确认扫描**：`confirm()` 遍历该笔业务对应的全部 `evm_tx_attempts` 记录（倒序优先查最新加价交易），任一 `tx_hash` 查到 Receipt 即视为该交易最终上链，回写主表最终 `tx_hash` 并按其 `GasUsed * EffectiveGasPrice` 结算实际费用。
3. **Nonce 越过检测**：发送或查询时若遇到 `nonce too low`，或者通过 `PendingNonceAt` / `NonceAt`（confirmed nonce）发现链上已确认 nonce `> item.Nonce`，先遍历检查全部 `evm_tx_attempts` 的 Receipt；若全部为 `ethereum.NotFound`，说明同 nonce 已被外部交易占用或丢失，立即迁移至 `MANUAL_REVIEW` 并写入 `worker_errors`。


---

## 9. 第二轮修复（2026-11-20 复核遗留）

> 复核基于 HEAD `085c2c5`。结论：A1/A3/A4/B3/C/D/E/F 主体正确；A2、A5、B1、B2 部分完成，另有新引入的阻塞问题。
> 现状：迁移实际已用到 `000008`（000005 为管理员鉴权，000008 为性能索引，与第 0 节表格不同，以实际为准）。本轮新迁移从 `000009` 开始，编号见各批次。
> 已存在迁移的 up 文件不得修改；仅允许按 J5 修改 `000007` 的 down 文件。

| 批次 | 内容 | 主要文件 | 迁移号 | 依赖 |
|------|------|----------|--------|------|
| G | BTC 遗留（含阻塞问题） | `internal/chain/bitcoin`、`internal/btc`、`store/postgres/bitcoin*.go` | 000010（如需要） | 无，最先做 |
| H | EVM 加价与 nonce 遗留 | `internal/withdrawal`、`internal/token/*/worker.go`、`chain/evm/worker_helpers.go`、`balance_withdrawal_transaction.go` | 000009 | 无 |
| I | 扫描器漏账风险 | `internal/deposit/scanner.go`、`internal/token/deposit/scanner.go`、`internal/btc/scanner_worker.go`、`repository.go` | 无 | BTC 扫描部分等 G 合并 |
| J | 鉴权与运维遗留 | `internal/app/auth_handler.go`、`app.go`、`internal/config`、`migrations/000007_*.down.sql` | 000011（如需要） | 无 |

G、H、J 可以并行；I 的 EVM 部分可以并行，BTC 部分等 G 合并后再做。

### 批次 G：BTC 遗留

- **G1（阻塞，最高优先级）BTC RPC 错误解析**
  - 问题：`internal/chain/bitcoin/client.go:219` 用的是 `jsonrpc 1.0`，Bitcoin Core 对这种请求出错时返回 HTTP 500；而 `:240` 遇到非 200 直接报错，不解析响应体。结果 -5（交易不存在）永远解析不出来，A2 新增的"按 txid 查链"在交易不存在时一律报错，SIGNED 和 BROADCAST_UNKNOWN 的提币、归集全部卡住。
  - 方案：非 200 时也尝试解析 body 中的 `error` 并返回 `*RPCError`，解析不了再报 HTTP 状态错误。`internal/btc/withdrawal_worker.go:195-197` 删除 `"not found"` 子串匹配，只认 code -5。部署文档写明节点需要 `txindex=1`。
  - 验收：用 `httptest` 模拟 Bitcoin Core 返回 HTTP 500 + `{"error":{"code":-5,...}}`，断言得到 `*RPCError{Code:-5}`；worker 测试改为走真实 client + httptest，而不是 fake 直接返回 RPCError。
- **G2 `ReleaseExpiredBTCUTXOLocks` 删除或收紧**（`store/postgres/bitcoin.go:671-677`）：它不看单据状态就释放过期锁。改为只释放 FAILED 单据或已不存在单据的锁；如果没有使用场景就直接删除，同时删除对应测试。**选择说明**：已选择直接删除 `ReleaseExpiredBTCUTXOLocks`（全仓没有任何调用方与测试依赖，且按租约过期释放会绕过单据状态检查，对已签名/广播未知提币带来重复选币风险）。
- **G3 完成时按快照消耗 UTXO**：`CompleteBTCWithdrawal`（`bitcoin_withdrawal_worker.go:377-382`）和 `CompleteBTCSweep`（`bitcoin.go:157`）改为按 `selected_inputs_json` 或归集记录里的 UTXO ID 更新为 SPENT，并要求 RowsAffected 等于输入数，不相等就回滚并转 MANUAL_REVIEW。
- **G4 vsize 估算**（`internal/btc/select.go:10-20`）：overhead 计入 segwit marker/flag，结果向上取整；补一个 1 输入 2 输出 = 141 vB 的单测。
- **G5 选币锁范围**（`bitcoin_withdrawal_worker.go:105-114`）：改为先不加锁按金额取有限条候选（例如前 50 条），再对选中的行做 `FOR UPDATE SKIP LOCKED` 并复核。
- **G6 归集 dust 收口**（`internal/btc/sweep_worker.go:90-93`）：金额不足以支付手续费或低于 dust 时，调用 `FailBTCSweep` 并写入一条 worker_errors，不再每轮重试。
- **G7 补测试**：
  - 存在 USER_DEPOSIT 的 UNSPENT UTXO 时，提币不会选中它；
  - `CompleteBTCWithdrawal` 拒绝 FAILED 和 CREATED 状态；
  - `CompleteBTCSweep` 重复调用是幂等的。

### 批次 H：EVM 加价与 nonce 遗留

- **H1 加价失败退回重播并且只告警一次**
  - 问题：`internal/withdrawal/worker.go:497-499,528-530`、`token/withdrawal/worker.go:586`、`token/sweep/worker.go:685`、`token/gasstation/worker.go:650` 中，加价时余额不足就 `return nil`。之后每轮都重新签名、写一条 worker_errors，而且不再重播原交易。`EVM_BUMP_FEE_CAP_EXCEEDED` 也是每轮都写。
  - 方案：
    - 加价前先校验余额，校验不过就不签名；
    - 加价失败后，如果达到重播间隔就走 `rebroadcast`；
    - 持久化 `bump_blocked_at` 和原因，同一原因只写一次 worker_errors；
    - 持续阻塞超过配置阈值后转 MANUAL_REVIEW。
  - 四条链路都要改，优先在 `worker_helpers.go` 里统一实现。
- **H2 归集加价补气**：归集需要加价而地址 ETH 不足时，先创建一笔追加补气任务（复用 Gas Station 流程），补气到账后再加价；或者把归集的最大加价次数限制在补气安全余量能覆盖的范围内，二选一，并在本节写明选择。**选择说明**：已选择方案二——将 Token 归集的有效最大加价次数约束在补气安全余量 `GasSafetyBPS` 可覆盖的范围内（按 `(1 + BumpPercent/100)^k <= (1 + GasSafetyBPS/10000)` 动态计算覆盖上限，默认 `GasSafetyBPS=2000` / `BumpPercent=15` 对应最多 1 次安全加价），避免修改已广播归集单据的补气状态机并发边界；若因链上 Base Fee 暴涨仍出现 ETH 不足，则统一走 H1 的 `bump_blocked_at` 单次告警、重播退回与超时人工核查。
- **H3 分配 nonce 后不再调用会失败的 RPC**
  - 问题：`withdrawal/worker.go:180`、`token/withdrawal/worker.go:190`、`token/sweep/worker.go:244` 在 `sign()` 里重新调用 EstimateGas。
  - 方案：
    - 在 prepare 阶段（分配 nonce 的同一个事务里）把 gasLimit、maxFee、tip 落库，sign 只读这些值做签名；
    - `worker_helpers.go:352-401` 的内存失败计数改为持久化，超过阈值后转 MANUAL_REVIEW，不要只在内存里暂停。
- **H4 重签不得超过冻结的预留费**（`withdrawal/worker.go:180-209`、`SaveSignedWithdrawal`）：恢复路径重签时，如果 `gasLimit×maxFee > reserved_fee_wei`，先调用 `IncreaseWithdrawalFee`；余额不足就把 feeCap 限制在冻结额以内。保证 `FinalizeWithdrawal` 不会出现 `ErrActualFeeExceedsHold`。
- **H5 迁移 000009 回填存量数据**：
  - 已有 tx_hash 但没有 attempt 记录的单子，插入 `attempt_no=1` 的 evm_tx_attempts；
  - `broadcasted_at` 和 `last_broadcast_at` 回填为 `COALESCE(..., updated_at)`；
  - 四张业务表都要处理；
  - down 只删除回填产生的数据。
- **H6 Gas Station 余额校验**（`token/gasstation/worker.go:231`）：分配 nonce 前，校验平台余额不少于 `补气金额 + gasLimit×maxFee + PlatformMinBalanceWei`。
- **H7 补测试**：
  - 广播时遇到 nonce too low；
  - ETH 加价时用户余额不足（断言只写一次错误并且会重播）；
  - 带存量数据执行迁移 000009；
  - insufficient funds 和 nonce too low 不触发端点切换。

### 批次 I：扫描器漏账风险

- **I1 地址增量加载会漏地址**（`repository.go:282`，`deposit/scanner.go:216-231`，token 扫描器 `:240`）
  - 问题：`id > lastAddressID` 依赖自增 ID 的提交顺序，并发事务下 ID 较小的地址可能晚提交，从而被永久跳过。
  - 方案：增量查询改为回看窗口，例如 `id > lastAddressID - 1000` 或 `created_at > 上次时间 - 5 分钟`，再在内存里去重；同时每 N 轮做一次全量刷新。
- **I2 Token 扫描逐条核对日志区块**（`token/deposit/scanner.go:374-376、389、437-445`）：getLogs 返回的每条日志，都要用对应块高的区块头核对 BlockHash，不一致就视为重组，本批不提交。末块区块头要在 getLogs 之后再校验一次。
- **I3 扫块失败不阻塞确认入账**（`deposit/scanner.go:183-185`、token 扫描器 `:205-207`、`btc/scanner_worker.go:111-127`）：扫块阶段出错后，本轮仍然执行 confirm 和入账阶段，只跳过扫块。
- **I4 BTC 回退的健壮性**（G 合并后做）：
  - `btc/scanner_worker.go:162-168`：查询回退目标块哈希失败时直接返回错误，只有回退成功才执行孤立；
  - `bitcoin.go:270`：单个用户待确认余额不足时，不要让整个回退失败，只把该笔转 MANUAL_REVIEW 并告警；
  - 回退时只孤立区块哈希确实变化了的充值。
- **I5 回退深度校验**（`config.go:222、238`）：启动时校验 `REORG_REWIND_BLOCKS ≥ 确认数`。
- **I6 重组核对不依赖本轮扫块**（`deposit/scanner.go:310`）：已入账充值的重组核对改为独立执行，分批循环直到处理完，不受 confirmBatchSize 限制，也不依赖本轮有没有扫块。

### 批次 J：鉴权与运维遗留

- **J1 登录限流和锁定**（`internal/app/auth_handler.go:74-105`）：按 IP + 用户名做令牌桶限流（参数从配置读取）；连续失败 N 次后锁定一段时间。锁定状态可以放内存或落库（落库用迁移 000011），在本节写明选择。
- **J2 防账号枚举**（`auth_handler.go:88-100`）：用户不存在时也和一个固定的 dummy hash 做 bcrypt 比较；先校验密码，再判断是否禁用，并且用户不存在、密码错误、账号被禁用统一返回同一个错误码。
- **J3 禁用后令牌立即失效**（`auth_handler.go:271-283`）：中间件验签后查询管理员是否 disabled，可以带短 TTL 的内存缓存；也可以在 token 里写入 `token_version`，禁用或改密时版本号加 1。
- **J4 健康检查不泄漏内部信息**（`app.go:338-340、581-617`）：未鉴权的 healthz/readyz 只返回状态枚举；`*_last_error` 和 `btcErr.Error()` 只放在需要鉴权的系统状态接口里。
- **J5 `000007` 的 down 改为显式拒绝**：down 中先检查是否存在 ORPHANED 充值或 DEPOSIT_REVERSAL 流水，存在就 `RAISE EXCEPTION` 说明不可回滚，不要直接删数据。
- **J6 小项**：
  - API Key 通道不要硬编码 `AdminID: 1`（`auth_handler.go:255`），改为配置项或专用的系统管理员；
  - API_KEY 要求至少 32 字节；
  - `ReadHeaderTimeout` 和关闭超时改为从配置读取（`app.go:154、455`）；
  - Shutdown 失败时用 `errors.Join` 保留先退出组件的错误（`app.go:454-457`）；
  - 等待 worker 超时后记录哪些组件没有退出；
  - 000008 的索引说明：生产大表要用 `CREATE INDEX CONCURRENTLY` 单独执行（在部署文档里写明）。

### 第二轮通用提示词补充（拼在第 1 节前缀之后）

```text
这是第二轮修复，针对的是复核发现的遗留问题，详见 docs/development/backend-remediation-plan.md 第 9 节。
1. 本轮新增迁移从 000009 开始，只使用分配给你的版本号；已存在迁移的 up 文件不得修改。
2. 每个问题先写一个能复现的失败测试（集成测试用真实 PostgreSQL：TEST_DATABASE_URL，本地可用 deploy/compose.yaml 启动的库），再修改代码。
3. 不要用 fake 直接返回期望的错误类型来"证明"修复。涉及 RPC 错误解析的测试，必须用 httptest 模拟真实节点的 HTTP 响应。
4. 完成后贴出 go vet ./...、go test ./...、TEST_DATABASE_URL=... go test -count=1 ./internal/store/postgres ./migrations 的结果。
   internal/wallet 的文件权限测试在 Windows 上本来就会失败，不需要处理，但要在报告里注明。
```

### 各批次提示词

```text
<第 1 节前缀> + <第二轮补充>
你负责批次 G（BTC 遗留），按 G1 → G7 完成。G1 是阻塞问题，先单独提交一次。G2 选择删除还是收紧，在第 9 节 G2 条目后补一句说明。
```

```text
<第 1 节前缀> + <第二轮补充>
你负责批次 H（EVM 加价与 nonce 遗留），按 H1 → H7 完成，迁移号使用 000009。
H1、H3 的改动优先放进 chain/evm/worker_helpers.go，四条链路（ETH 提币、Token 提币、Token 归集、Gas Station）统一使用，不要每条链路各写一份。
H2 先在第 9 节写明选择的方案，再实现。
```

```text
<第 1 节前缀> + <第二轮补充>
你负责批次 I（扫描器漏账风险），先完成 I1、I2、I3（EVM 部分）、I5、I6。
I3 的 BTC 部分和 I4 要等批次 G 合并后再做：开始前用 git log 确认，没有合并就停下来告诉用户。
每一项都要有一个测试，证明在修复前会漏账或卡住。
```

```text
<第 1 节前缀> + <第二轮补充>
你负责批次 J（鉴权与运维遗留），按 J1 → J6 完成；如果需要新表，迁移号使用 000011。
J1 的锁定状态放内存还是落库，先在第 9 节写明，再实现。
修改登录接口的错误码时同步更新 docs/api.md，并列出前端受影响的位置，但不要修改前端代码。
```
