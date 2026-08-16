# Mini Custody HTTP API

## 1. 通用约定

- 基础地址：`http://localhost:8080/api/v1`
- Content-Type：`application/json`
- 所有 ETH、Wei、Token 十进制金额和 Token 最小单位均为 JSON 字符串，禁止按浮点数解析。
- 时间使用 RFC 3339，并带 `+08:00` 时区偏移。
- 每个响应包含 `X-Request-ID` 响应头。
- 分页参数为 `page` 和 `page_size`，默认 `1` 和 `20`，`page_size` 最大为 `100`。

分页响应格式：

```json
{
  "items": [],
  "page": 1,
  "page_size": 20,
  "has_more": false
}
```

错误响应格式：

```json
{
  "code": "NOT_FOUND",
  "message": "请求的记录不存在",
  "request_id": "32位请求标识"
}
```

## 2. 接口列表

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/users` | 查询演示用户 |
| GET | `/assets` | 查询链上资产配置 |
| GET | `/users/{user_id}/wallet` | 查询用户充值地址和余额 |
| GET | `/users/{user_id}/balances` | 查询用户全部资产余额 |
| GET | `/users/{user_id}/deposits` | 分页查询用户充值 |
| GET | `/users/{user_id}/token-deposits` | 分页查询用户 Token 充值 |
| GET | `/users/{user_id}/withdrawals` | 分页查询用户提币 |
| POST | `/users/{user_id}/withdrawal-quote` | 只读估算提币最大费用 |
| POST | `/users/{user_id}/withdrawals` | 幂等创建提币 |
| POST | `/users/{user_id}/token-withdrawal-quote` | 只读试算 Token 提币和平台 ETH Gas |
| POST | `/users/{user_id}/token-withdrawals` | 幂等创建 Token 提币 |
| GET | `/users/{user_id}/token-withdrawals` | 分页查询用户 Token 提币 |
| GET | `/withdrawals/{withdrawal_id}` | 查询单笔提币 |
| GET | `/token-withdrawals/{withdrawal_id}` | 查询单笔 Token 提币 |
| GET | `/transactions` | 分页查询全局资产和任务流水 |
| GET | `/sweeps` | 分页查询 Token 归集任务 |
| GET | `/internal-transfers` | 分页查询平台 Gas 补充转账 |
| GET | `/system/platform-wallet` | 查询平台热钱包公开状态 |
| GET | `/system/chains/sepolia` | 查询 Sepolia 和扫描状态 |
| GET | `/worker-errors` | 分页查询 Worker 错误 |

## 3. 钱包余额

资产列表和用户多资产余额：

```text
GET /api/v1/assets
GET /api/v1/users/1/balances
```

多资产余额中的 `available`、`pending_deposit` 和 `pending_withdrawal` 是按资产 `decimals` 格式化的十进制字符串；对应的 `*_units` 字段保留最小单位整数。ERC-20 资产同时返回合约地址和精度。

请求：

```text
GET /api/v1/users/1/wallet
```

响应示例：

```json
{
  "user_id": 1,
  "network": "ethereum-sepolia",
  "address": "0xD3197b634d458724c41e3019AD06a8224ad07571",
  "balance": {
    "asset": "ETH",
    "available_wei": "976580409736000",
    "available_eth": "0.000976580409736",
    "pending_deposit_wei": "0",
    "pending_deposit_eth": "0",
    "pending_withdrawal_wei": "0",
    "pending_withdrawal_eth": "0",
    "updated_at": "2026-08-15T11:10:13+08:00"
  }
}
```

## 4. 创建提币

请求必须携带同一用户范围内唯一的 `Idempotency-Key`：

```bash
curl -X POST http://localhost:8080/api/v1/users/1/withdrawals \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: withdrawal-business-id' \
  -d '{
    "to_address": "0xb043cE34da539a714A92D47258f36188d8DEcb4e",
    "amount_eth": "0.001"
  }'
```

首次创建返回 `201` 和 `created=true`。相同键、目标地址和金额的重试返回原提币、`200` 和 `created=false`。相同键用于不同地址或金额时返回 `409 IDEMPOTENCY_CONFLICT`。

## 5. Token 提币试算与创建

试算不会占用余额。`amount` 按资产 `decimals` 精确转换，响应同时返回十进制金额、最小单位整数以及平台预计承担的最大 ETH Gas：

```bash
curl -X POST http://localhost:8080/api/v1/users/1/token-withdrawal-quote \
  -H 'Content-Type: application/json' \
  -d '{
    "to_address": "0xb043cE34da539a714A92D47258f36188d8DEcb4e",
    "amount": "1.5"
  }'
```

创建请求必须携带用户范围内唯一的 `Idempotency-Key`：

```bash
curl -X POST http://localhost:8080/api/v1/users/1/token-withdrawals \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: token-withdrawal-business-id' \
  -d '{
    "to_address": "0xb043cE34da539a714A92D47258f36188d8DEcb4e",
    "amount": "1.5"
  }'
```

用户只占用 Token 金额，ETH Gas 由平台热钱包承担。平台 Token 库存或 ETH Gas 不足时，Worker 不签名并保留任务重试；广播超时或进程重启后只重播数据库中相同的 `raw_tx`，API 从不返回该字段。

## 6. 浏览器链接

充值、提币和全局交易响应中的 `explorer_url` 指向 Sepolia Etherscan 交易页面。充值响应还包含对应的 `block_url`。未产生交易哈希时不返回浏览器链接。

统一流水支持资产和类型筛选：

```text
GET /api/v1/transactions?page=1&page_size=20&asset=USDC&type=TOKEN_DEPOSIT
```

`type` 可选值为 `DEPOSIT`、`WITHDRAWAL`、`TOKEN_DEPOSIT`、`TOKEN_WITHDRAWAL`、`TOKEN_SWEEP` 和 `GAS_TOPUP`。

运维页面使用以下只读接口：

```text
GET /api/v1/sweeps?page=1&page_size=20
GET /api/v1/internal-transfers?page=1&page_size=20
GET /api/v1/system/platform-wallet
```

平台热钱包接口只返回地址、Nonce、ETH/Token 余额快照和健康状态，不返回密钥、派生路径或已签名交易。

## 7. 安全边界

API 不返回以下内容：

- 助记词、Seed 或私钥；
- 密钥库密码；
- 完整 RPC URL 或 RPC Key；
- 已签名交易的 `raw_tx`；
- PostgreSQL 原始错误。
