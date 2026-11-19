import type {
  AdminUser,
  ApiErrorBody,
  Asset,
  BitcoinDeposit,
  BitcoinSweep,
  BitcoinWallet,
  BitcoinWithdrawal,
  ChainStatus,
  CreateUserInput,
  CreateUserResponse,
  Deposit,
  InternalTransfer,
  LoginResponse,
  MultiAssetBalance,
  Page,
  PlatformWallet,
  TokenDeposit,
  TokenSweep,
  TokenWithdrawal,
  TokenWithdrawalQuote,
  Transaction,
  UpdateUserInput,
  User,
  Wallet,
  Withdrawal,
  WithdrawalQuote,
  WorkerError,
} from './types'

const AUTH_TOKEN_KEY = 'mini-custody-admin-token'
const AUTH_ADMIN_KEY = 'mini-custody-admin-profile'
export const UNAUTHORIZED_EVENT = 'mini-custody:unauthorized'

export function getAuthToken(): string | null {
  try {
    return localStorage.getItem(AUTH_TOKEN_KEY)
  } catch {
    return null
  }
}

export function getStoredAdmin(): AdminUser | null {
  try {
    const raw = localStorage.getItem(AUTH_ADMIN_KEY)
    return raw ? (JSON.parse(raw) as AdminUser) : null
  } catch {
    return null
  }
}

export function setAuthSession(token: string, admin?: AdminUser): void {
  try {
    localStorage.setItem(AUTH_TOKEN_KEY, token)
    if (admin) {
      localStorage.setItem(AUTH_ADMIN_KEY, JSON.stringify(admin))
    }
  } catch {
    // ignore storage errors
  }
}

export function clearAuthSession(): void {
  try {
    localStorage.removeItem(AUTH_TOKEN_KEY)
    localStorage.removeItem(AUTH_ADMIN_KEY)
  } catch {
    // ignore storage errors
  }
}

function handleUnauthorized(): void {
  clearAuthSession()
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent(UNAUTHORIZED_EVENT))
    if (window.location.pathname !== '/login') {
      window.history.replaceState(null, '', '/login')
    }
  }
}

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
  const isLoginPath = path === '/api/v1/auth/login'
  const token = !isLoginPath && path.startsWith('/api/v1/') ? getAuthToken() : null

  const response = await fetch(path, {
    ...init,
    headers: {
      Accept: 'application/json',
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...init?.headers,
    },
  })

  const body = (await response.json().catch(() => null)) as ApiErrorBody | T | null

  if (!response.ok) {
    const errorBody = (body as ApiErrorBody | null) ?? {
      code: 'INVALID_RESPONSE',
      message: '服务器响应无效',
    }
    if (
      !isLoginPath &&
      (response.status === 401 ||
        errorBody.code === 'UNAUTHORIZED' ||
        errorBody.code === 'TOKEN_EXPIRED')
    ) {
      handleUnauthorized()
    }
    throw new ApiError(response.status, errorBody)
  }

  return body as T
}

export const api = {
  login: (input: { username: string; password: string }) =>
    request<LoginResponse>('/api/v1/auth/login', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  me: () => request<AdminUser>('/api/v1/auth/me'),
  users: () => request<{ items: User[] }>('/api/v1/users'),
  createUser: (input: CreateUserInput) =>
    request<CreateUserResponse>('/api/v1/users', {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  updateUser: (userId: number, input: UpdateUserInput) =>
    request<User>(`/api/v1/users/${userId}`, {
      method: 'PATCH',
      body: JSON.stringify(input),
    }),
  assets: () => request<{ items: Asset[] }>('/api/v1/assets'),
  balances: (userId: number) => request<{ items: MultiAssetBalance[] }>(`/api/v1/users/${userId}/balances`),
  wallet: (userId: number) => request<Wallet>(`/api/v1/users/${userId}/wallet`),
  bitcoinWallet: (userId: number) => request<BitcoinWallet>(`/api/v1/users/${userId}/btc-wallet`),
  bitcoinDeposits: (userId: number, page = 1, pageSize = 20) =>
    request<Page<BitcoinDeposit>>(`/api/v1/users/${userId}/btc-deposits?page=${page}&page_size=${pageSize}`),
  bitcoinSweeps: (page = 1, pageSize = 20) =>
    request<Page<BitcoinSweep>>(`/api/v1/btc/sweeps?page=${page}&page_size=${pageSize}`),
  bitcoinWithdrawal: (id: number) => request<BitcoinWithdrawal>(`/api/v1/btc-withdrawals/${id}`),
  bitcoinWithdrawals: (userId: number, page = 1, pageSize = 20) =>
    request<Page<BitcoinWithdrawal>>(`/api/v1/users/${userId}/btc-withdrawals?page=${page}&page_size=${pageSize}`),
  createBitcoinWithdrawal: (userId: number, key: string, input: { to_address: string; amount_sats: number }) =>
    request<BitcoinWithdrawal>(`/api/v1/users/${userId}/btc-withdrawals`, {
      method: 'POST',
      headers: { 'Idempotency-Key': key },
      body: JSON.stringify(input),
    }),
  deposits: (userId: number, page = 1, pageSize = 20) =>
    request<Page<Deposit>>(`/api/v1/users/${userId}/deposits?page=${page}&page_size=${pageSize}`),
  withdrawals: (userId: number, page = 1, pageSize = 20) =>
    request<Page<Withdrawal>>(`/api/v1/users/${userId}/withdrawals?page=${page}&page_size=${pageSize}`),
  withdrawal: (withdrawalId: number) => request<Withdrawal>(`/api/v1/withdrawals/${withdrawalId}`),
  transactions: (page = 1, pageSize = 20, asset = '', type = '') =>
    request<Page<Transaction>>(
      `/api/v1/transactions?page=${page}&page_size=${pageSize}&asset=${encodeURIComponent(asset)}&type=${encodeURIComponent(type)}`,
    ),
  tokenDeposits: (userId: number, page = 1, pageSize = 20) =>
    request<Page<TokenDeposit>>(`/api/v1/users/${userId}/token-deposits?page=${page}&page_size=${pageSize}`),
  tokenWithdrawals: (userId: number, page = 1, pageSize = 20) =>
    request<Page<TokenWithdrawal>>(`/api/v1/users/${userId}/token-withdrawals?page=${page}&page_size=${pageSize}`),
  tokenWithdrawal: (id: number) => request<TokenWithdrawal>(`/api/v1/token-withdrawals/${id}`),
  sweeps: (page = 1, pageSize = 20) => request<Page<TokenSweep>>(`/api/v1/sweeps?page=${page}&page_size=${pageSize}`),
  internalTransfers: (page = 1, pageSize = 20) =>
    request<Page<InternalTransfer>>(`/api/v1/internal-transfers?page=${page}&page_size=${pageSize}`),
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
    request<TokenWithdrawalQuote>(`/api/v1/users/${userId}/token-withdrawal-quote`, {
      method: 'POST',
      body: JSON.stringify(input),
    }),
  createTokenWithdrawal: (userId: number, idempotencyKey: string, input: { to_address: string; amount: string }) =>
    request<TokenWithdrawal>(`/api/v1/users/${userId}/token-withdrawals`, {
      method: 'POST',
      headers: { 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify(input),
    }),
}
