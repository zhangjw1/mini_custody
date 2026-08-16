# 002 - Sepolia ERC-20 托管钱包需求

## 1. 目的

在现有 Sepolia ETH 托管钱包上增加一种可配置的标准 ERC-20 测试资产，完成 Token 充值、确认、Gas 补充、归集、热钱包统一提币和 Web 展示闭环。

本阶段重点体现交易所托管钱包与普通链上钱包的区别：用户资产以平台账本为准，充值地址只负责收款，确认后的 Token 自动归集到平台热钱包，提币统一由热钱包发送。

## 2. 范围

本阶段包含：

- Ethereum Sepolia 公开测试网；
- 一个通过配置启用的标准 ERC-20 测试 Token；
- 复用现有用户 Sepolia 充值地址；
- 基于 `Transfer` Event 的 Token 充值扫描；
- Token 确认和幂等账本入账；
- 平台热钱包和链上 Token 库存；
- 用户充值地址 Gas 补充；
- 用户地址向热钱包归集 Token；
- 热钱包统一处理 Token 提币；
- Web 页面按资产查看余额、充值、提币、归集和链上状态；
- RPC 故障、服务重启和重复处理恢复。

本阶段不包含：

- 主网或真实资产；
- 多个 Token 同时启用；
- fee-on-transfer、rebasing、ERC-777 或非标准 Token；
- DEX、闪兑或 Gas 费用折算为 Token；
- 冷钱包、人工审核、HSM、MPC；
- 自动链重组回滚；
- Solana、Bitcoin 和跨链桥。

## 3. 关键产品规则

### 3.1 Token 配置

- Token 合约地址必须通过配置明确提供，不能根据 symbol 自动发现；
- 启动时必须读取并校验 `symbol`、`decimals` 和合约代码；
- 第一阶段只允许启用一个测试 Token；
- API 金额使用十进制字符串，数据库使用最小单位整数；
- 前端展示 symbol 和 decimals，但不得依赖前端完成精度换算。

### 3.2 充值与归集

1. 用户向现有 Sepolia 充值地址发送 Token；
2. 系统扫描 Token 合约的 `Transfer` Event；
3. 达到确认数后增加用户 Token 可用余额；
4. 系统检查用户地址是否有足够 ETH 支付归集 Gas；
5. Gas 不足时由平台 Gas Station 补充最小必要 ETH；
6. 用户地址签署 Token `transfer`，将全部可归集 Token 转入热钱包；
7. 归集完成后更新平台热钱包 Token 库存状态。

归集失败不得撤销已经确认的用户账本余额。用户账本表示平台负债，归集只是平台链上资金管理过程。

### 3.3 提币

1. 用户选择 Token、目标地址和金额；
2. 系统展示 Token 数量，平台暂不向用户收取链上 Gas；
3. 系统占用用户 Token 可用余额；
4. 热钱包检查 Token 库存和 ETH Gas 余额；
5. 热钱包构建、签名并广播 Token `transfer`；
6. 达到确认数后结算用户 Token 余额，并记录平台实际 ETH Gas 成本；
7. 明确广播失败时释放用户占用余额；
8. 广播结果不明确时持续跟踪原交易，不得换 Nonce 或重复创建交易。

### 3.4 内部 ETH 转账

Gas Station 向用户充值地址发送的 ETH 属于平台内部资金，不得被现有 ETH 充值扫描器记入用户 ETH 余额。

系统必须在广播 Gas 补充交易前持久化原始交易和交易哈希。ETH 扫描器发现匹配哈希时，将其识别为内部转账并跳过用户充值入账。

## 4. 功能需求

### 4.1 资产与合约

- **ERC20-AST-001**：系统必须支持配置 Token 合约地址、symbol、decimals 和启用状态。
- **ERC20-AST-002**：启动时必须验证合约地址存在字节码。
- **ERC20-AST-003**：链上读取的 symbol、decimals 必须与配置一致，否则拒绝启动 Token Worker。
- **ERC20-AST-004**：所有 Token 数值必须使用整数精度，禁止二进制浮点数。
- **ERC20-AST-005**：Token 合约地址统一保存为规范化小写地址，API 使用 EIP-55 展示。

### 4.2 Token 充值

- **ERC20-DEP-001**：扫描器必须按区块范围调用日志查询，只接受配置合约发出的 `Transfer(address,address,uint256)` Event。
- **ERC20-DEP-002**：`to` 为已分配用户地址且 `value > 0` 时创建 Token 充值。
- **ERC20-DEP-003**：充值必须保存交易哈希、日志索引、区块高度、区块哈希、地址和最小单位金额。
- **ERC20-DEP-004**：唯一标识必须包含网络、合约地址、交易哈希和日志索引。
- **ERC20-DEP-005**：充值达到配置确认数后只能增加一次用户 Token 可用余额。
- **ERC20-DEP-006**：重复日志、重复区块扫描和服务重启不得重复入账。
- **ERC20-DEP-007**：RPC 失败时检查点不得越过失败区块范围。
- **ERC20-DEP-008**：入账前必须复核充值区块哈希；不一致时暂停自动入账并记录错误。

Token 充值生命周期：

```text
DETECTED -> CONFIRMING -> CONFIRMED -> CREDITED
```

### 4.3 Gas 补充

- **ERC20-GAS-001**：系统必须估算一次 Token 归集所需 Gas，并检查用户地址 ETH 余额。
- **ERC20-GAS-002**：Gas Station 只补充完成归集所需的最小金额和安全余量。
- **ERC20-GAS-003**：同一归集任务不得创建多笔并发 Gas 补充交易。
- **ERC20-GAS-004**：Gas 补充必须使用数据库 Nonce 分配、EIP-1559 签名和相同原始交易重播。
- **ERC20-GAS-005**：Gas 补充交易必须登记为内部转账，禁止增加用户 ETH 账本余额。
- **ERC20-GAS-006**：Gas Station ETH 余额不足时暂停新任务并在 Web 后台展示告警。

### 4.4 Token 归集

- **ERC20-SWP-001**：每笔已确认 Token 充值必须能够创建幂等归集任务。
- **ERC20-SWP-002**：归集金额以签名前用户地址最新 Token 余额为准，但不得超过系统已识别的可归集金额。
- **ERC20-SWP-003**：归集交易必须从用户地址签名，目标固定为平台热钱包。
- **ERC20-SWP-004**：归集必须保存 Nonce、原始交易、交易哈希、Gas 参数和状态。
- **ERC20-SWP-005**：广播后重启必须继续跟踪原交易，不得产生第二笔不同交易。
- **ERC20-SWP-006**：归集完成后必须保存实际 ETH Gas 成本。
- **ERC20-SWP-007**：归集失败不得扣减用户 Token 账本余额。

归集生命周期：

```text
CREATED -> WAITING_GAS -> SIGNING -> SIGNED -> BROADCASTED -> CONFIRMING -> COMPLETED
    |            |          |            |
    +------------+----------+------------+-> FAILED
```

### 4.5 Token 提币

- **ERC20-WDR-001**：提币必须包含用户、目标地址、Token 资产和大于零的十进制金额。
- **ERC20-WDR-002**：系统必须使用 Token decimals 将金额精确转换为最小单位。
- **ERC20-WDR-003**：超过用户可用 Token 余额的请求必须在签名前拒绝。
- **ERC20-WDR-004**：相同用户和幂等标识最多只能创建一笔 Token 提币。
- **ERC20-WDR-005**：提币从平台热钱包统一出款，不使用用户充值地址。
- **ERC20-WDR-006**：热钱包 Token 或 ETH Gas 不足时不得签名，任务保持可恢复状态并产生告警。
- **ERC20-WDR-007**：交易必须调用配置 Token 合约的标准 `transfer(address,uint256)`。
- **ERC20-WDR-008**：广播和重启恢复必须复用相同 Nonce、原始交易和交易哈希。
- **ERC20-WDR-009**：链上成功后扣除用户占用 Token，并记录平台实际 ETH Gas 成本。
- **ERC20-WDR-010**：链上执行失败时释放用户 Token 金额，但平台仍记录实际 Gas 成本。

### 4.6 Web 后台

- **ERC20-UI-001**：顶部资产选择器支持 ETH 和配置 Token。
- **ERC20-UI-002**：账户页展示不同资产的可用、待确认和处理中余额。
- **ERC20-UI-003**：充值页明确展示网络、Token 合约地址、symbol 和 decimals。
- **ERC20-UI-004**：交易流水区分 ETH 充值、Token 充值、归集、Gas 补充和 Token 提币。
- **ERC20-UI-005**：运维区域展示热钱包 Token 库存、Gas Station ETH 余额和失败任务。
- **ERC20-UI-006**：地址、合约和交易哈希均提供 Sepolia 浏览器链接。

## 5. 安全与失败要求

- 只允许 Chain ID `11155111`；
- Token 合约不匹配时不得启动 Token Worker；
- 合约调用的 calldata、地址、金额必须在签名前重新校验；
- 私钥、助记词、RPC Key 和解密密码不得进入 API、日志或任务错误；
- Gas 补充、归集和提币必须分别使用幂等键及持久化 Nonce；
- RPC 超时或限流不得伪造交易失败；
- 热钱包库存不足不得导致用户账本变成负数；
- 普通自动测试不得依赖公开 RPC、测试币或已部署 Token。

## 6. 验收场景

### 场景 A：Token 配置

- 使用正确合约启动并读取 symbol、decimals；
- 使用错误合约地址或错误 decimals 启动时，Token Worker 拒绝运行。

### 场景 B：Token 充值

- 向 Alice 地址发送测试 Token；
- 验证日志被发现、等待确认并只入账一次；
- 重启和重复扫描后余额不重复增加。

### 场景 C：Gas 补充与归集

- Alice 地址没有足够 ETH；
- Gas Station 发送内部 ETH；
- ETH 扫描器不增加 Alice ETH 余额；
- Token 全部归集到热钱包并记录实际 Gas。

### 场景 D：热钱包 Token 提币

- Alice 提交有效 Token 提币；
- Token 余额被占用；
- 热钱包完成 `transfer` 并确认；
- Alice Token 余额和平台 Gas 成本准确结算。

### 场景 E：故障恢复

- 在 Gas 补充、归集或提币广播后重启服务；
- 系统继续处理原交易；
- 不产生重复内部 ETH 入账、重复归集或重复提币。

## 7. 完成标准

- 需求场景 A-E 均有自动测试和一次真实 Sepolia 演示证据；
- ETH 原有充提流程不回归；
- Web 页面能够完成 Token 充值地址查看、Token 提币和状态观察；
- Token 充值完成后能够自动归集；
- 所有普通测试不访问公网；
- 项目文档包含测试 Token、热钱包、Gas Station 和测试币准备步骤。
