# Mini Custody Bitcoin Signet 充提设计

> 网络兼容更新：运行时通过 `BITCOIN_NETWORK=signet|testnet4` 单选一个测试网络。单个进程不会同时运行两套 BTC 网络；RPC `chain` 和数据库 network 必须与配置一致。

## 1. 范围

本阶段为现有 Mini Custody 设计 Bitcoin Signet 充值和提币，先完成 Go 后端核心模型、RPC 适配器、UTXO 扫描器和交易构建器，再接入现有 Web 后台。

不做以下内容：

- Bitcoin 主网和 Bitcoin Testnet4；
- 本地 regtest 作为最终验收网络；
- Ordinals、BRC-20、闪电网络和多签；
- 直接依赖 Bitcoin Core 钱包保存私钥；
- 用账户余额模型替代 UTXO 模型。

Bitcoin Core 通过 `-signet` 或 `-chain=signet` 选择 Signet，默认 RPC 端口为 `38332`。正式测试只使用公开 Signet 节点或自建 Signet 节点，不把本地 regtest 当作 Signet 的替代品。

## 2. 目标产品行为

### 2.1 充值

每个用户拥有一个确定性的 Signet Native SegWit 充值地址。外部地址向该地址发送 BTC 后：

1. 充值扫描器从持久化检查点开始扫描新区块；
2. 解析每笔交易的输出，匹配已登记的充值地址；
3. 每个命中的 `txid:vout` 创建唯一充值记录；
4. 未达到确认数时进入 `CONFIRMING`，只计入待确认余额；
5. 达到确认数后进入 `CREDITED`，增加用户可用 BTC 余额；
6. 充值入账后创建可选的归集任务，把用户地址 UTXO 转入平台热钱包。

充值记录的业务唯一键是 `(network, txid, vout, address_id)`，不能只使用 txid，因为一笔 Bitcoin 交易可以包含多个命中输出。

### 2.2 提币

用户提交目标地址和 BTC 金额后：

1. 校验目标地址属于 Signet 网络；
2. 按 satoshi 精确解析金额，不使用浮点数；
3. 锁定用户账本中的可用余额；
4. 从平台热钱包 UTXO 集合选择输入；
5. 构建一个或多个目标输出和找零输出；
6. 使用估算费率计算手续费，保证输入总额 >= 目标金额 + 手续费；
7. 保存 unsigned transaction/PSBT 摘要、输入 UTXO、输出和费率；
8. 使用托管密钥离线签名，签名前复核输入、目标地址、金额、找零地址和手续费；
9. 广播后保存 txid，按 `BROADCASTED -> CONFIRMING -> COMPLETED` 更新；
10. 链上失败或广播状态不明确时，根据同一原始交易恢复，不重新选择输入和找零。

## 3. 密钥和地址

### 3.1 地址类型

第一阶段统一使用 BIP84 P2WPKH Native SegWit 地址，Signet 使用 testnet 风格地址前缀 `tb1`。不混用 Legacy `m/44'`、Nested SegWit `m/49'` 和 Taproot `m/86'`。

### 3.2 派生路径

```text
m/84'/1'/0'/0/{user_index}     # Signet/testnet coin type 1，接收地址
m/84'/1'/0'/1/{change_index}    # 找零地址
```

用户充值地址按用户索引稳定派生。提币找零地址从平台热钱包自己的 change 分支派生，不能把找零发送到用户充值地址。

数据库需要保存：

- `network`、`address`、`script_pub_key`；
- `derivation_index`、`derivation_path`；
- 地址类型 `P2WPKH`；
- 地址用途 `USER_DEPOSIT` 或 `PLATFORM_CHANGE`；
- 是否启用和最后扫描高度。

API 只返回地址和网络，不返回 xprv、助记词、私钥或完整扩展私钥。

## 4. 数据库模型

### 4.1 `btc_addresses`

| 字段 | 说明 |
|---|---|
| `id` | 自增主键 |
| `user_id` | 用户 ID，平台找零地址为空 |
| `network` | 固定 `bitcoin-signet` |
| `purpose` | `USER_DEPOSIT` / `PLATFORM_CHANGE` |
| `address` | Bech32 地址 |
| `script_pub_key` | 输出脚本十六进制 |
| `derivation_index` | 派生索引 |
| `derivation_path` | 派生路径 |
| `created_at` / `updated_at` | UTC+8 展示时间 |

唯一约束：`(network, address)`、`(network, purpose, derivation_index)`。

### 4.2 `btc_deposits`

一条命中输出对应一条记录：

```text
id, user_id, address_id, network,
txid, vout, block_hash, block_height,
amount_sats, confirmations, status,
created_at, updated_at
```

唯一约束：`(network, txid, vout, address_id)`。金额全部使用 `BIGINT` satoshi。

### 4.3 `btc_utxos`

保存可花费输出和锁定状态：

```text
id, address_id, txid, vout, value_sats, script_pub_key,
block_height, spend_txid, status, locked_by, locked_until,
created_at, updated_at
```

状态至少包括 `UNSPENT`、`LOCKED`、`SPENT`、`UNKNOWN`。唯一约束：`(network, txid, vout)`。

### 4.4 `btc_withdrawals`

```text
id, user_id, idempotency_key, to_address,
amount_sats, fee_sats, change_sats, fee_rate_sat_vb,
selected_inputs_json, outputs_json, psbt_hash,
raw_tx_hash, txid, block_height, confirmations,
status, error_code, error_message,
created_at, updated_at
```

数据库只保存审计需要的哈希、输入输出摘要和状态。原始 PSBT/签名交易是否落库要按密钥安全策略决定；API 永远不返回它们。

### 4.5 `btc_sweeps`

充值入账后为对应 UTXO 幂等创建归集任务：

```text
id, deposit_id, utxo_id, from_address_id, to_address_id,
input_value_sats, output_value_sats, fee_sats, fee_rate_sat_vb,
raw_tx, txid, block_height, confirmations, status,
error_code, error_message, created_at, updated_at
```

同一 UTXO 只能关联一笔归集。`raw_tx` 必须在广播前落库，API 不返回该字段。归集是平台资金位置变化，不扣减已经入账的用户 BTC 余额，也不写用户余额扣款流水。

## 5. RPC 适配器

第一阶段支持 Bitcoin Core JSON-RPC：

| 能力 | RPC |
|---|---|
| 网络和高度 | `getblockchaininfo` |
| 区块 | `getblockhash`、`getblock` |
| 交易详情 | `getrawtransaction` |
| 费率 | `estimatesmartfee` |
| 广播 | `sendrawtransaction` |
| 交易验证 | `testmempoolaccept` |

不依赖 Bitcoin Core wallet RPC 的 `listunspent`、`sendtoaddress` 和 `dumpprivkey` 完成业务。UTXO 索引由系统自己维护，签名由 Go 密钥模块完成。

RPC 客户端必须具备：

- Signet Chain 校验；
- 请求超时、429、连接失败和节点切换；
- 区块扫描范围限制；
- `getrawtransaction` 失败不能当作“没有交易”；
- 广播超时进入 `BROADCAST_UNKNOWN`，不能直接重新构建交易。

## 6. 充值扫描和恢复

扫描检查点：

```text
network=bitcoin-signet
scanner=btc-deposit
last_scanned_height
last_scanned_hash
```

每轮扫描使用 `[checkpoint + 1, min(checkpoint + batch_size, tip)]`。区块处理事务内完成：

1. 读取区块交易和输出；
2. 根据 `script_pub_key` 命中地址；
3. `INSERT ... ON CONFLICT DO NOTHING` 写入充值和 UTXO；
4. 推进检查点；
5. 提交事务。

重启、重复区块、重复交易和同交易多个输出都依赖唯一约束保证幂等。确认数使用 `tip_height - block_height + 1`。检测到区块哈希变化时，回退至少一个可配置确认窗口，标记受影响充值为 `REORG_RECHECK` 后重新扫描。

## 7. 提币选币和交易构建

### 7.1 选币

MVP 使用确定性 Branch-and-Bound 的简化版本：

1. 排除 `LOCKED`、`SPENT` 和确认数不足的 UTXO；
2. 按确认高度升序、金额降序排序；
3. 优先选择刚好覆盖金额和预估手续费的最少输入；
4. 找不到精确组合时使用最小超额组合；
5. 仍不足时返回余额不足，不创建半成品提币。

所有被选 UTXO 在同一数据库事务中加锁，并设置租约。服务重启后，过期租约可释放；已广播提币的输入不能释放，必须根据 txid 恢复。

### 7.2 输出和找零

- 主输出：用户目标地址和 `amount_sats`；
- 找零输出：平台 `PLATFORM_CHANGE` 地址；
- 若找零小于 dust threshold，则并入手续费，不生成找零输出；
- 不允许向用户充值地址自动找零；
- 输出顺序和脚本摘要写入数据库，签名前后必须一致。

### 7.3 手续费

手续费使用 sat/vB 费率和交易虚拟大小估算。MVP 先支持 P2WPKH 输入/输出，估算公式保守取上限：

```text
fee_sats = ceil(estimated_vbytes * fee_rate_sat_vb)
```

后续可接入 `estimatesmartfee`，但必须配置最小费率、最大费率和最大绝对手续费，防止 RPC 异常导致过高费用。

## 8. 状态机

### 充值

```text
DETECTED -> CONFIRMING -> CONFIRMED -> CREDITED
                         \-> REORG_RECHECK
```

### 提币

```text
CREATED -> INPUTS_LOCKED -> SIGNING -> SIGNED
       -> BROADCASTING -> BROADCAST_UNKNOWN
       -> BROADCASTED -> CONFIRMING -> COMPLETED
                         \-> FAILED
```

### 归集

```text
CREATED -> SIGNING -> SIGNED -> BROADCAST_UNKNOWN
                         \-> BROADCASTED -> CONFIRMING -> COMPLETED
                                                   \-> FAILED
```

归集只消费已达到确认数且状态为 `UNSPENT` 的用户充值 UTXO，目标固定为平台 `PLATFORM_CHANGE` 地址。费用从该 UTXO 的链上输出金额中扣除；扣费后低于 dust threshold 的任务标记失败并保留 UTXO，不广播交易。

所有状态迁移必须带当前状态条件。失败释放 UTXO 锁和用户余额占用；`BROADCAST_UNKNOWN` 不释放输入、不换交易内容，先通过 txid/raw transaction 查询确认结果。

## 9. MVP 优先级

1. Signet 配置、RPC 客户端和 Chain 校验；
2. BIP84 地址派生和地址表；
3. 区块扫描、输出识别、UTXO 和充值入账；
4. UTXO 选币、P2WPKH 交易构建、找零和手续费；
5. 离线签名、`testmempoolaccept`、广播和提币状态机；
6. 重启恢复、区块重组回退和幂等测试；
7. Web 资产选择、BTC 充值地址、提币表单和流水页；
8. Signet 真实 E2E。

## 10. 关键验收标准

- 同一 `txid:vout` 重复扫描只生成一条充值和一条 UTXO；
- 一笔交易两个命中输出分别入账，金额和 vout 不混淆；
- 未确认充值不能进入可用余额；
- 并发提币不会重复选择同一个 UTXO；
- 找零永远进入平台 change 地址；
- 广播超时不会重复广播不同交易；
- 重启后能恢复扫描点、UTXO 锁和广播中提币；
- 区块重组不会重复入账；
- Web 页面不接触私钥、RPC 凭据或原始签名交易。
