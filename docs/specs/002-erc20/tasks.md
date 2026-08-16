# 002 - Sepolia ERC-20 托管钱包任务清单

## 1. 使用规则

- 本清单从 `001-evm-wallet` 的 T078 继续编号；
- 每次只实现一个能够独立测试的纵向切片；
- 所有金额使用 Token 最小单位整数，API 使用字符串；
- 每个 Go 方法必须有中文注释，日志和对外错误使用中文；
- 普通测试禁止访问公开 RPC 或消耗测试币；
- 未确定测试 Token 合约前，不开始真实 Sepolia 端到端任务；
- 所有真实链上操作只允许 Sepolia。

## 2. Phase 1：范围、Token 与配置

- [x] **T079** 评审 `002-erc20` 需求和技术设计，确认第一阶段只启用一个标准测试 Token。
- [x] **T080** 选择 Sepolia 测试 Token，记录合约地址、symbol、decimals、获取方式和区块浏览器链接。
- [x] **T081** 已选择 Circle 官方 Sepolia USDC，本阶段不部署可铸造 Demo Token。
- [x] **T082** 增加 ERC-20、归集、Gas 补充和平台热钱包配置结构。
- [x] **T083** 增加配置校验，限制 Chain ID、合约地址、decimals、Gas 安全余量和单次补充上限。
- [x] **T084** 更新 `deploy/env.example`，只添加无敏感值变量名和安全默认值。
- [x] **T085** 添加最小标准 ERC-20 ABI，并用测试向量校验方法 ID 和 Transfer Topic。

### Phase 1 验收

- 配置关闭时现有 ETH 系统行为不变；
- 配置错误时 Token Worker 拒绝启动，但错误不泄露 RPC URL 或密钥；
- Token 合约选择和测试币来源有明确记录；
- ABI 编解码完全由结构化库完成。

## 3. Phase 2：资产与数据库模型

- [x] **T086** 新增 `assets` 表和 ETH 初始资产迁移数据。
- [x] **T087** 为 `asset_balances` 增加资产关联，同时保持现有 ETH 数据兼容。
- [x] **T088** 新增 `platform_wallets` 表并初始化 `HOT` 派生地址。
- [x] **T089** 新增 `token_deposits` 表及 Event 唯一约束。
- [x] **T090** 新增 `internal_transfers` 表保存 Gas 补充原始交易和状态。
- [x] **T091** 新增 `token_sweeps` 表及同地址同资产活动任务约束。
- [x] **T092** 新增 `token_withdrawals` 表及用户幂等约束。
- [x] **T093** 扩展余额流水允许 `TOKEN_DEPOSIT` 和 `TOKEN_WITHDRAWAL` 引用类型。
- [x] **T094** 为所有新表和字段添加中文数据库注释、检查约束和查询索引。
- [x] **T095** 实现新模型的 Store 查询和扫描方法。
- [x] **T096** 编写迁移、模型和约束集成测试。

### Phase 2 验收

- 原有 ETH 地址、余额、充值和提币数据迁移后保持不变；
- 一个用户可以同时拥有 ETH 和 Token 余额；
- Token Event、活动归集、平台 Nonce 和提币幂等由数据库约束兜底；
- `initial.down` 或新增 down migration 能完整回滚本阶段结构。

## 4. Phase 3：ERC-20 RPC 与合约适配器

- [x] **T097** 扩展 EVM Client 的 `CodeAt`、`CallContract` 和 `FilterLogs` 能力。
- [x] **T098** 实现合约 `symbol()`、`decimals()` 和 `balanceOf()` 查询。
- [x] **T099** 实现标准 `transfer(address,uint256)` calldata 编码和结果校验。
- [x] **T100** 实现 `Transfer` Event 严格解码，校验 topics、data、地址和金额。
- [x] **T101** 实现 Token transfer Gas 估算。
- [x] **T102** 启动时校验合约代码、symbol 和 decimals。
- [x] **T103** 为日志范围过大、429、超时和备用 RPC 增加错误分类与重试测试。
- [x] **T104** 编写 Mock JSON-RPC 合约测试，覆盖无效 ABI、revert、false 返回和 Removed Log。

### Phase 3 验收

- Token 合约调用不使用手工字符串拼接 calldata；
- 错误合约和错误元数据无法启动 Token Worker；
- RPC 日志查询失败不会被解释为空结果；
- Mock 测试不依赖公开 RPC。

## 5. Phase 4：Token 充值扫描与入账

- [x] **T105** 实现按区块范围扫描配置合约 Transfer Event。
- [x] **T106** 在内存地址索引中过滤用户目标地址，并按区块和日志索引排序。
- [x] **T107** 实现 Token 充值、pending 余额和检查点原子提交。
- [x] **T108** 实现 Token 充值确认数更新和区块哈希复核。
- [x] **T109** 实现 `CONFIRMED -> CREDITED` 幂等入账和余额流水。
- [x] **T110** 入账后幂等创建 Token 归集任务。
- [x] **T111** 实现服务重启从 Token 检查点下一高度继续扫描。
- [x] **T112** 编写重复日志、重复区块、同交易多 Event 和并发入账测试。
- [x] **T113** 编写 RPC 临时失败时检查点和用户余额不变化测试。
- [x] **T114** 将 Token 网络高度、扫描高度、落后区块和错误接入健康状态。

### Phase 4 验收

- 一笔真实或 Mock Transfer Event 只创建一条充值；
- 达到确认数前 Token 不进入可用余额；
- 重启、重复扫描和并发执行不重复入账；
- Token 扫描错误可以通过 API 和 Web 观察。

## 6. Phase 5：Gas Station 与内部转账

- [x] **T115** 实现平台热钱包地址初始化和派生一致性校验。
- [x] **T116** 实现归集 Gas 需求计算、用户地址 ETH 余额检查和补充上限。
- [x] **T117** 实现平台热钱包 Nonce 数据库行锁分配。
- [x] **T118** 构建和签名 EIP-1559 Gas 补充交易。
- [x] **T119** 在广播前保存 internal transfer 的 raw_tx 和 tx_hash。
- [x] **T120** 实现 Gas 补充广播、Receipt 确认、实际费用和重启恢复。
- [x] **T121** 修改 ETH 充值扫描器，匹配 internal transfer tx_hash 时跳过用户入账。
- [x] **T122** 实现 Gas Station ETH 余额阈值和不足告警。
- [x] **T123** 编写内部 ETH 转账不会增加用户 ETH 余额的数据库集成测试。
- [x] **T124** 编写 Gas 补充重复执行、广播超时和相同 raw_tx 重播测试。

### Phase 5 验收

- 用户地址 Gas 不足时只补充受上限约束的必要 ETH；
- 内部 Gas 补充在 ETH 充值扫描中可见但不记入用户余额；
- 平台 Nonce 不重复；
- 广播结果不明确时不换 Nonce、不释放任务。

## 7. Phase 6：Token 自动归集

- [x] **T125** 实现归集任务查询、抢占和状态迁移。
- [x] **T126** 实现同地址新充值合并到未签名归集任务。
- [x] **T127** 签名前读取用户地址最新 Token 和 ETH 余额。
- [x] **T128** 使用用户地址 Nonce 构建 Token transfer 归集交易。
- [x] **T129** 签名前解码 calldata，复核 Token 合约、热钱包目标和金额。
- [x] **T130** 广播前保存归集 raw_tx、tx_hash 和 Gas 参数。
- [x] **T131** 实现归集广播、确认、实际 Gas 和重启恢复。
- [x] **T132** 实现归集失败和热钱包到账异常告警，不修改用户 Token 账本。
- [x] **T133** 实现热钱包 Token 链上库存快照。
- [x] **T134** 编写同地址并发归集、Nonce、重复广播和失败恢复测试。

### Phase 6 验收

- 已确认 Token 可以从用户地址自动转入平台热钱包；
- 同地址同资产同时只有一个执行中的归集；
- 归集失败不会减少用户账本余额；
- 重启后只跟踪和重播原始归集交易。

## 8. Phase 7：热钱包 Token 提币

- [x] **T135** 实现 Token 十进制金额和最小单位双向精确转换。
- [x] **T136** 实现 Token 提币试算接口，返回 Token 金额和平台预计 ETH Gas。
- [x] **T137** 实现用户 Token 余额占用和幂等提币创建。
- [x] **T138** 实现热钱包 Token 库存和 ETH Gas 余额检查。
- [x] **T139** 使用平台热钱包共享 Nonce 构建 Token transfer 提币交易。
- [x] **T140** 广播前保存并复核提币 calldata、raw_tx 和 tx_hash。
- [x] **T141** 实现广播、Receipt 确认和实际 ETH Gas 记录。
- [x] **T142** Receipt 成功时结算用户 Token 占用余额。
- [x] **T143** Receipt 失败时释放用户 Token，并保留平台 Gas 成本。
- [x] **T144** 实现广播后重启恢复和相同交易重播。
- [x] **T145** 编写余额不足、库存不足、Gas 不足、幂等和并发超花测试。

### Phase 7 验收

- Token 提币统一从平台热钱包发送；
- 用户 Token 余额不会因并发请求变负；
- 平台 Gas 成本和用户 Token 账本分开记录；
- API 重试和 Worker 重启不会产生重复链上交易。

## 9. Phase 8：API 与 Web 后台

- [x] **T146** 实现资产列表和用户多资产余额 API。
- [x] **T147** 实现 Token 充值、提币详情和列表 API。
- [x] **T148** 实现 Sweep、Internal Transfer 和平台钱包状态 API。
- [x] **T149** 扩展统一 Transactions 投影，支持 Token、Sweep 和 Gas Top-up。
- [x] **T150** 前端增加资产选择器，并保持用户选择状态。
- [x] **T151** Account 和 Deposit 页面展示 Token 余额、合约和 decimals。
- [x] **T152** Withdraw 页面支持 Token 试算、提交和生命周期轮询。
- [x] **T153** Transactions 页面增加 Token 和任务类型筛选。
- [x] **T154** 新增 Operations 页面展示热钱包库存、Gas、归集和失败任务。
- [x] **T155** 完成加载、空数据、RPC 异常、库存不足和长错误信息状态。
- [x] **T156** 完成桌面和移动宽度浏览器验收。

### Phase 8 验收

- 用户可以通过 Web 完成 Token 地址查看和提币；
- 运维人员可以观察充值、Gas 补充、归集和热钱包出款；
- 浏览器不访问 RPC、不签名、不接触密钥；
- ETH 页面和 API 不回归。

## 10. Phase 9：Sepolia 端到端与文档

- [x] **T157** 编写测试 Token、热钱包 ETH 和外部测试地址准备说明，并提供只读环境预检命令。
- [ ] **T158** 完成真实 Token 充值、确认、幂等入账验收。
- [ ] **T159** 完成真实 Gas 补充，并验证没有产生用户 ETH 入账。
- [ ] **T160** 完成真实 Token 自动归集和实际 Gas 验收。
- [ ] **T161** 完成真实热钱包 Token 提币和账本结算验收。
- [ ] **T162** 在 Gas 补充、归集和提币广播后分别完成重启恢复验收。
- [ ] **T163** 模拟 RPC 429、超时和临时不一致，确认所有检查点和余额安全。
- [ ] **T164** 运行完整 ETH 回归测试和 ERC-20 普通测试。
- [ ] **T165** 完成 API 文档、README、架构图和演示步骤。

### Phase 9 验收

- 需求文档场景 A-E 全部通过；
- Token 从外部充值到用户账本、归集、热钱包提币形成完整闭环；
- ETH 充值和提币继续正常；
- 普通测试不连接公网；
- 真实验收可以通过显式配置重复执行，不需要人工修改数据库。

## 11. 下一阶段

完成 `002-erc20` 后进入：

1. `003-solana-wallet`：Solana Devnet SOL 充值和提币；
2. `004-bitcoin-wallet`：Bitcoin Signet UTXO 充提；
3. `006-resilience`：重叠扫描、自动对账、重组恢复和多实例 Worker。
