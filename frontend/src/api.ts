import type {
  ApiErrorBody,
  Asset,
  ChainStatus,
  Deposit,
  InternalTransfer,
  MultiAssetBalance,
  Page,
  PlatformWallet,
  TokenDeposit,
  TokenSweep,
  TokenWithdrawal,
  TokenWithdrawalQuote,
  Transaction,
  User,
  Wallet,
  Withdrawal,
  WithdrawalQuote,
  WorkerError,
} from './types'

export class ApiError extends Error {
  status: number
  code: string
  requestId?: string

  constructor(status: number, body: ApiErrorBody) {
    super(body.message || '请求失败')
    this.name = 'ApiError'
    this.status = status
    this.code = body.code || 'UNKNOWN_ERROR'
    this.requestId = body.request_id
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: {
      Accept: 'application/json',
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...init?.headers,
    },
  })
  const body = await response.json().catch(() => null)
  if (!response.ok) {
    throw new ApiError(response.status, body ?? { code: 'INVALID_RESPONSE', message: '服务器响应无效' })
  }
  return body as T
}

export const api = {
  users: () => request<{ items: User[] }>('/api/v1/users'),
  assets: () => request<{ items: Asset[] }>('/api/v1/assets'),
  balances: (userId: number) => request<{ items: MultiAssetBalance[] }>(`/api/v1/users/${userId}/balances`),
  wallet: (userId: number) => request<Wallet>(`/api/v1/users/${userId}/wallet`),
  deposits: (userId: number, page = 1, pageSize = 20) =>
    request<Page<Deposit>>(`/api/v1/users/${userId}/deposits?page=${page}&page_size=${pageSize}`),
  withdrawals: (userId: number, page = 1, pageSize = 20) =>
    request<Page<Withdrawal>>(`/api/v1/users/${userId}/withdrawals?page=${page}&page_size=${pageSize}`),
  withdrawal: (withdrawalId: number) => request<Withdrawal>(`/api/v1/withdrawals/${withdrawalId}`),
  transactions: (page = 1, pageSize = 20, asset = '', type = '') =>
    request<Page<Transaction>>(`/api/v1/transactions?page=${page}&page_size=${pageSize}&asset=${encodeURIComponent(asset)}&type=${encodeURIComponent(type)}`),
  tokenDeposits: (userId: number, page = 1, pageSize = 20) =>
    request<Page<TokenDeposit>>(`/api/v1/users/${userId}/token-deposits?page=${page}&page_size=${pageSize}`),
  tokenWithdrawals: (userId: number, page = 1, pageSize = 20) =>
    request<Page<TokenWithdrawal>>(`/api/v1/users/${userId}/token-withdrawals?page=${page}&page_size=${pageSize}`),
  tokenWithdrawal: (id: number) => request<TokenWithdrawal>(`/api/v1/token-withdrawals/${id}`),
  sweeps: (page = 1, pageSize = 20) => request<Page<TokenSweep>>(`/api/v1/sweeps?page=${page}&page_size=${pageSize}`),
  internalTransfers: (page = 1, pageSize = 20) => request<Page<InternalTransfer>>(`/api/v1/internal-transfers?page=${page}&page_size=${pageSize}`),
  platformWallet: () => request<PlatformWallet>('/api/v1/system/platform-wallet'),
  chain: () => request<ChainStatus>('/api/v1/system/chains/sepolia'),
  workerErrors: (page = 1, pageSize = 20) =>
    request<Page<WorkerError>>(`/api/v1/worker-errors?page=${page}&page_size=${pageSize}`),
  quoteWithdrawal: (userId: number, input: { to_address: string; amount_eth: string }) =>
    request<WithdrawalQuote>(`/api/v1/users/${userId}/withdrawal-quote`, {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  createWithdrawal: (
    userId: number,
    idempotencyKey: string,
    input: { to_address: string; amount_eth: string },
  ) =>
    request<Withdrawal>(`/api/v1/users/${userId}/withdrawals`, {
      method: 'POST',
      headers: { 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify(input),
    }),
  quoteTokenWithdrawal: (userId: number, input: { to_address: string; amount: string }) =>
    request<TokenWithdrawalQuote>(`/api/v1/users/${userId}/token-withdrawal-quote`, { method: 'POST', body: JSON.stringify(input) }),
  createTokenWithdrawal: (userId: number, idempotencyKey: string, input: { to_address: string; amount: string }) =>
    request<TokenWithdrawal>(`/api/v1/users/${userId}/token-withdrawals`, { method: 'POST', headers: { 'Idempotency-Key': idempotencyKey }, body: JSON.stringify(input) }),
}
