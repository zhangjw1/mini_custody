export type User = {
  id: number
  code: string
  display_name: string
  created_at: string
}

export type Balance = {
  asset: string
  available_wei: string
  available_eth: string
  pending_deposit_wei: string
  pending_deposit_eth: string
  pending_withdrawal_wei: string
  pending_withdrawal_eth: string
  updated_at: string
}

export type Wallet = {
  user_id: number
  network: string
  address: string
  balance: Balance
  created_at: string
}

export type Deposit = {
  id: number
  user_id: number
  network: string
  asset: string
  tx_hash: string
  explorer_url: string
  block_number: number
  block_url: string
  amount_wei: string
  amount_eth: string
  confirmations: number
  status: string
  created_at: string
  updated_at: string
}

export type Withdrawal = {
  id: number
  user_id: number
  to_address: string
  amount_eth: string
  amount_wei: string
  reserved_fee_wei: string
  reserved_fee_eth: string
  actual_fee_wei?: string
  actual_fee_eth?: string
  gas_limit: number
  max_fee_per_gas_wei: string
  max_priority_fee_per_gas_wei: string
  status: string
  created: boolean
  tx_hash?: string
  explorer_url?: string
  confirmations: number
  nonce?: string
  block_number?: number
  error_code?: string
  error_message?: string
  created_at: string
  updated_at: string
}

export type Transaction = {
  type: 'DEPOSIT' | 'WITHDRAWAL'
  id: number
  user_id: number
  asset: string
  tx_hash?: string
  explorer_url?: string
  amount_wei: string
  amount_eth: string
  block_number?: number
  confirmations: number
  status: string
  created_at: string
  updated_at: string
}

export type ChainStatus = {
  network: string
  status: string
  endpoint: string
  chain_id: string
  network_height: number
  scan_height: number
  lag: number
  last_error?: string
  checked_at: string
}

export type WorkerError = {
  id: number
  worker: string
  stage: string
  reference_type?: string
  reference_id?: number
  error_code: string
  error_message: string
  retry_count: number
  first_occurred_at: string
  last_occurred_at: string
}

export type Page<T> = {
  items: T[]
  page: number
  page_size: number
  has_more: boolean
}

export type WithdrawalQuote = {
  amount_eth: string
  amount_wei: string
  gas_limit: number
  max_fee_per_gas_wei: string
  max_priority_fee_per_gas_wei: string
  reserved_fee_wei: string
  reserved_fee_eth: string
}

export type ApiErrorBody = {
  code: string
  message: string
  request_id?: string
}
