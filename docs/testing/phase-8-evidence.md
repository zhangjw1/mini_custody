# Phase 8 验收证据

验收日期：2026-08-15

## 已完成

### A. 确定性地址

- 使用同一加密根密钥重启后端。
- 用户 1 和用户 2 的地址在重启前后逐字一致，且两个地址不同。
- 用户 1 地址为 `0xD3197b634d458724c41e3019AD06a8224ad07571`。
- 重启后用户 1 余额、充值和提币记录保持不变。

### B. 真实充值和重复扫描

- 交易：[Sepolia Etherscan](https://sepolia.etherscan.io/tx/0x7aa7a5984230f6bdf4d70267474cb08120a459e3db3f5e9d1cb3c47d3bda1e9b)
- 到账金额：`0.002 ETH`。
- 区块：`11491276`。
- 达到 `3` 个确认后状态为 `CREDITED`。
- 可用余额增加为 `0.002 ETH`，待确认余额归零。
- 重启补扫后仍只有一条充值、一条待确认流水和一条入账流水，余额没有重复增加。

### C. 真实提币和费用结算

- 交易：[Sepolia Etherscan](https://sepolia.etherscan.io/tx/0x269734416482480057a442d0a0b6023c79c7d28de83f860e4ee708d536fe07fd)
- 提币金额：`0.001 ETH`。
- 收款地址：`0xb043cE34da539a714A92D47258f36188d8DEcb4e`。
- Nonce：`0`，Gas Used：`21000`。
- 实际网络费：`0.000023419590264 ETH`。
- 达到 `3` 个确认后状态为 `COMPLETED`，待提币余额归零。
- 最终可用余额：`0.000976580409736 ETH`。
- 使用同一幂等键重试返回 `200`、`created=false`，未产生第二笔提币或广播。

### D. 故障保护

- `TestScannerDoesNotAdvanceCheckpointAfterTemporaryRPCFailure` 验证读取目标区块失败时扫描点、充值和余额不变化，RPC 恢复后从同一高度重试。
- `TestWorkerResumesBroadcastedWithdrawalAfterRestart` 验证新 Worker 接管已广播记录后只查询原交易，`raw_tx` 和交易哈希不变化且不再次广播。
- Mock RPC 契约测试覆盖 `429`、超时、`503` 主备切换和错误 Chain ID。

## 待完成

需求场景 E 的真实链上“广播后、确认前重启”尚未执行。当前已有自动化恢复测试，但不能替代真实广播窗口的操作证据，因此 T076 保持未完成。

完成时应补充：提币 ID、交易哈希、Nonce、停止与重启时间、重启前后状态、数据库唯一记录计数和 Etherscan 链接；不得记录 `raw_tx` 内容、密钥、密码或完整 RPC URL。
