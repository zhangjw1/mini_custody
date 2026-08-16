# Mini Custody HTTP API

## 1. 通用约定

- 基础地址：`http://localhost:8080/api/v1`
- Content-Type：`application/json`
- 所有 ETH 和 Wei 金额均为 JSON 字符串，禁止按浮点数解析。
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
| GET | `/users/{user_id}/wallet` | 查询用户充值地址和余额 |
| GET | `/users/{user_id}/deposits` | 分页查询用户充值 |
| GET | `/users/{user_id}/withdrawals` | 分页查询用户提币 |
| POST | `/users/{user_id}/withdrawal-quote` | 只读估算提币最大费用 |
| POST | `/users/{user_id}/withdrawals` | 幂等创建提币 |
| GET | `/withdrawals/{withdrawal_id}` | 查询单笔提币 |
| GET | `/transactions` | 分页查询全局充值和提币 |
| GET | `/system/chains/sepolia` | 查询 Sepolia 和扫描状态 |
| GET | `/worker-errors` | 分页查询 Worker 错误 |

## 3. 钱包余额

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

## 5. 浏览器链接

充值、提币和全局交易响应中的 `explorer_url` 指向 Sepolia Etherscan 交易页面。充值响应还包含对应的 `block_url`。未产生交易哈希时不返回浏览器链接。

## 6. 安全边界

API 不返回以下内容：

- 助记词、Seed 或私钥；
- 密钥库密码；
- 完整 RPC URL 或 RPC Key；
- 已签名交易的 `raw_tx`；
- PostgreSQL 原始错误。
