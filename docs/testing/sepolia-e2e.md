# Sepolia 端到端验收手册

## 1. 范围和安全边界

本手册用于 Phase 8 的 Sepolia 原生 ETH 验收。所有密钥、RPC Key 和资产必须专用于测试网，不得复用生产、主网或个人钱包凭据。

- `deploy/env.example` 只保存变量结构，不填写真实凭据。
- 真实值只写入已被 Git 忽略的 `deploy/config.local.yaml`，文件权限设为 `600`。
- `backend/e2e` 是只读证据测试，不创建充值、提币或数据库记录。
- 测试不自动请求水龙头；测试币由操作者提前准备。
- 助记词只在离线终端初始化时显示一次，不得放入文档、Issue、聊天或 CI 日志。

如果真实密码、助记词或带 Provider Key 的 RPC URL 曾进入 Git 历史，应先轮换，再继续使用环境。

## 2. 环境准备

要求 Go `1.23.5`、PostgreSQL `17+`、Docker Compose 和可用的 Sepolia RPC。建议准备主、备两个不同 Provider 的 RPC 端点。

从仓库根目录启动 PostgreSQL：

```bash
docker compose -f deploy/compose.yaml up -d postgres
```

创建或编辑本地 YAML 配置并限制权限：

```bash
${EDITOR:-vi} deploy/config.local.yaml
chmod 600 deploy/config.local.yaml
```

至少填写以下变量：

```yaml
DATABASE_URL: 'postgres://...'
CUSTODY_KEYSTORE_FILE=../secrets/custody-root.age
CUSTODY_KEYSTORE_PASSWORD=...
SEPOLIA_RPC_URL=https://...
SEPOLIA_RPC_FALLBACK_URL=https://...
SEPOLIA_SCAN_START_BLOCK=...
```

首次创建专用测试根密钥：

```bash
cd backend
export CONFIG_FILE=../deploy/config.local.yaml
go run ./cmd/walletgen init --out ../secrets/custody-root.age
go run ./cmd/walletgen verify --file ../secrets/custody-root.age --index 1
go run ./cmd/walletgen verify --file ../secrets/custody-root.age --index 2
```

`init` 会输出一次助记词，应立即离线备份并清理终端历史。两个派生地址必须不同。只向用户 1 地址准备满足一次小额充值、一次小额提币和 Gas 的 Sepolia ETH。

## 3. 启动应用

```bash
cd backend
export CONFIG_FILE=../deploy/config.local.yaml
make run
```

另一个终端启动 Web：

```bash
cd frontend
npm install
npm run dev
```

后端健康检查为 `http://127.0.0.1:8080/healthz`，Web 为 `http://127.0.0.1:5173/`。链状态必须显示 Chain ID `11155111`，扫描高度最终应追平网络高度。

## 4. 只读证据测试

应用和 PostgreSQL 运行后，指定已经完成的真实交易：

```bash
cd backend
export CONFIG_FILE=../deploy/config.local.yaml
export E2E_USER_ID=1
export E2E_EXPECTED_WALLET_ADDRESS='0x...'
export E2E_DEPOSIT_TX_HASH='0x...'
export E2E_WITHDRAWAL_TX_HASH='0x...'
export E2E_CONFIRMATIONS=3
make e2e-test
```

可选变量：

- `E2E_API_BASE_URL`，默认 `http://127.0.0.1:8080`；
- `E2E_DATABASE_URL`，默认回退到 `DATABASE_URL`；
- `E2E_SEPOLIA_RPC_URL`，默认回退到 `SEPOLIA_RPC_URL`。

测试校验以下不变量：

- RPC Chain ID、API 地址和数据库地址一致；
- 充值交易、Receipt、区块、金额、扫描点和账本记录一致；
- 重复扫描后充值、待确认流水和入账流水各只有一份；
- 提币保存的 `raw_tx` 哈希、收款地址和金额与链上交易一致；
- `gas_used * effective_gas_price` 等于数据库实际费用；
- 提币幂等键、交易哈希、余额占用和最终结算各只有一份。

普通 `make test` 只编译并跳过该套件，不依赖数据库、公网 RPC 或测试币。

## 5. A-E 演示步骤

### A. 确定性地址

1. 记录 `/api/v1/users/1/wallet` 和 `/api/v1/users/2/wallet` 的地址。
2. 正常停止后端并使用同一密钥文件和密码重启。
3. 再次读取两个接口，确认各自地址逐字一致且两者互不相同。

### B. 确认充值

1. 从外部 Sepolia 钱包向用户 1 地址发送小额 ETH，保存交易哈希。
2. 在达到配置确认数前确认充值为 `CONFIRMING`，金额只在待确认余额中。
3. 达到确认数后确认状态为 `CREDITED`，可用余额只增加一次。
4. 重启后端并等待扫描器追平，再运行只读证据测试确认没有重复记录或入账。

### C. 成功提币

1. 在 Web 中输入另一个 Sepolia 地址和小额金额，先检查最大费用报价。
2. 提交后确认可用余额减少、待提币余额增加，并出现交易哈希。
3. 达到确认数后确认状态为 `COMPLETED`、待提币余额为零。
4. 运行只读证据测试核对 Receipt、收款金额、实际 Gas 和账本。

### D. 无效和幂等提币

1. 提交非法地址，确认返回 `400` 且没有提币记录或交易哈希。
2. 提交超过可用余额的金额，确认返回 `409` 且没有签名或广播。
3. 使用相同 `Idempotency-Key`、地址和金额重试，确认返回原记录且 `created=false`。
4. 使用同一键但不同内容重试，确认返回 `409 IDEMPOTENCY_CONFLICT`。

### E. 广播后重启恢复

该场景会真实移动测试币，必须人工选择测试金额和目标地址。

1. 创建提币并等待状态进入 `BROADCASTED` 或 `CONFIRMING`，记录 ID、Nonce、交易哈希和 `raw_tx` 的数据库哈希。
2. 在达到所需确认数前停止后端，不修改数据库。
3. 使用同一配置重启，等待原记录进入 `COMPLETED`。
4. 确认交易哈希、Nonce 和持久化 `raw_tx` 未变化，链上只有该交易，数据库只有一条提币和一条最终结算流水。

自动化测试 `TestWorkerResumesBroadcastedWithdrawalAfterRestart` 验证恢复逻辑不会再次广播，但 Phase 8 只有完成上述真实广播窗口演练后才算全部通过。
