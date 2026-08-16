# 前端技术决策

## 结论

第一阶段 Web SPA 使用 React、TypeScript、Vite、Ant Design、TanStack Query 和 React Router。

## 原因

- React 和 TypeScript 适合按 Dashboard、Account、Deposit、Withdraw、Transactions 拆分工作视图；
- Ant Design 提供后台系统需要的表格、表单、状态、抽屉和响应式组件；
- TanStack Query 统一管理 API 缓存、轮询、失败和重新获取；
- Vite 只负责前端开发和构建，开发代理将 `/api` 转发给现有 Go 单体服务；
- 浏览器不访问 Sepolia RPC，不读取密钥，不执行签名。

前端是同一仓库中的 `frontend/` 目录，不增加独立业务服务或微服务。
