import type {
  ApiErrorBody,
  ChainStatus,
  Deposit,
  Page,
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
  wallet: (userId: number) => request<Wallet>(`/api/v1/users/${userId}/wallet`),
  deposits: (userId: number, page = 1, pageSize = 20) =>
    request<Page<Deposit>>(`/api/v1/users/${userId}/deposits?page=${page}&page_size=${pageSize}`),
  withdrawals: (userId: number, page = 1, pageSize = 20) =>
    request<Page<Withdrawal>>(`/api/v1/users/${userId}/withdrawals?page=${page}&page_size=${pageSize}`),
  withdrawal: (withdrawalId: number) => request<Withdrawal>(`/api/v1/withdrawals/${withdrawalId}`),
  transactions: (page = 1, pageSize = 20) =>
    request<Page<Transaction>>(`/api/v1/transactions?page=${page}&page_size=${pageSize}`),
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
}
