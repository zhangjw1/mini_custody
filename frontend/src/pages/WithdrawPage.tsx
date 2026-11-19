import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  AlertOctagon,
  ArrowUpFromLine,
  Calculator,
  Check,
  CheckCircle2,
  Clock,
  ExternalLink,
} from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import {
  addETH,
  AlertBanner,
  CopyButton,
  formatDate,
  formatETH,
  HashLink,
  PageHeader,
  Panel,
  QueryState,
  SectionHeader,
  StatusBadge,
} from '../components/Common'
import { Button } from '../components/ui/Button'
import { Modal } from '../components/ui/Modal'
import { useToast } from '../components/ui/Toast'
import type { TokenWithdrawal, TokenWithdrawalQuote, Withdrawal, WithdrawalQuote } from '../types'
import BitcoinWithdrawPanel from './BitcoinWithdrawPanel'

type WithdrawForm = { to_address: string; amount: string }
type Quote = { kind: 'ETH'; data: WithdrawalQuote } | { kind: 'TOKEN'; data: TokenWithdrawalQuote }
type Created = { kind: 'ETH'; data: Withdrawal } | { kind: 'TOKEN'; data: TokenWithdrawal }

const STEPS = ['已创建', '签名广播', '链上确认', '完成']

export default function WithdrawPage() {
  const { userId, currentUser, asset } = useAppContext()
  const queryClient = useQueryClient()
  const toast = useToast()

  const [toAddress, setToAddress] = useState('')
  const [amount, setAmount] = useState('')
  const [fieldErrors, setFieldErrors] = useState<{ to_address?: string; amount?: string }>({})
  const [quote, setQuote] = useState<Quote>()
  const [pendingValues, setPendingValues] = useState<WithdrawForm>()
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [created, setCreated] = useState<Created>()

  const isToken = asset.asset_type === 'ERC20'
  const isUserDisabled = currentUser?.status === 'DISABLED'

  const balancesQuery = useQuery({
    queryKey: ['balances', userId],
    queryFn: () => api.balances(userId),
    enabled: userId > 0,
    refetchInterval: 10_000,
  })

  const selectedBalance = useMemo(
    () => balancesQuery.data?.items.find((item) => item.asset === asset.symbol),
    [asset.symbol, balancesQuery.data],
  )

  const detailQuery = useQuery({
    queryKey: ['withdrawal-detail', created?.kind, created?.data.id],
    queryFn: async () =>
      created?.kind === 'TOKEN'
        ? { kind: 'TOKEN' as const, data: await api.tokenWithdrawal(created.data.id) }
        : { kind: 'ETH' as const, data: await api.withdrawal(created!.data.id) },
    enabled: Boolean(created?.data.id),
    refetchInterval: (query) =>
      ['COMPLETED', 'FAILED'].includes(query.state.data?.data.status ?? '') ? false : 3_000,
  })

  const current = detailQuery.data ?? created

  useEffect(() => {
    setQuote(undefined)
    setPendingValues(undefined)
    setCreated(undefined)
    setConfirmOpen(false)
    setToAddress('')
    setAmount('')
    setFieldErrors({})
  }, [asset.symbol, userId])

  useEffect(() => {
    if (detailQuery.data && ['COMPLETED', 'FAILED'].includes(detailQuery.data.data.status)) {
      queryClient.invalidateQueries({ queryKey: ['balances', userId] })
      queryClient.invalidateQueries({ queryKey: ['wallet', userId] })
      queryClient.invalidateQueries({ queryKey: ['transactions'] })
      queryClient.invalidateQueries({ queryKey: ['withdrawals', userId] })
      queryClient.invalidateQueries({ queryKey: ['token-withdrawals', userId] })
    }
  }, [detailQuery.data, queryClient, userId])

  const quoteMutation = useMutation({
    mutationFn: async (values: WithdrawForm): Promise<Quote> =>
      isToken
        ? {
            kind: 'TOKEN',
            data: await api.quoteTokenWithdrawal(userId, {
              to_address: values.to_address,
              amount: values.amount,
            }),
          }
        : {
            kind: 'ETH',
            data: await api.quoteWithdrawal(userId, {
              to_address: values.to_address,
              amount_eth: values.amount,
            }),
          },
    onSuccess: (data, values) => {
      setQuote(data)
      setPendingValues(values)
    },
  })

  const createMutation = useMutation({
    mutationFn: async (values: WithdrawForm): Promise<Created> =>
      isToken
        ? {
            kind: 'TOKEN',
            data: await api.createTokenWithdrawal(userId, crypto.randomUUID(), {
              to_address: values.to_address,
              amount: values.amount,
            }),
          }
        : {
            kind: 'ETH',
            data: await api.createWithdrawal(userId, crypto.randomUUID(), {
              to_address: values.to_address,
              amount_eth: values.amount,
            }),
          },
    onSuccess: (data) => {
      setConfirmOpen(false)
      setCreated(data)
      setPendingValues(undefined)
      toast.success(`${asset.symbol} 提币请求已提交`)
      queryClient.invalidateQueries({ queryKey: ['balances', userId] })
      queryClient.invalidateQueries({ queryKey: ['wallet', userId] })
    },
  })

  if (asset.network.startsWith('bitcoin-')) return <BitcoinWithdrawPanel userId={userId} />

  const validateForm = (): WithdrawForm | null => {
    const cleanAddr = toAddress.trim()
    const cleanAmt = amount.trim()
    const nextErrors: { to_address?: string; amount?: string } = {}

    if (!cleanAddr) {
      nextErrors.to_address = '请输入到账地址'
    } else if (!/^0x[a-fA-F0-9]{40}$/.test(cleanAddr)) {
      nextErrors.to_address = '请输入有效的 EVM 地址（0x 开头共 42 位十六进制）'
    }

    const amountPattern = new RegExp(`^(?:0|[1-9]\\d*)(?:\\.\\d{1,${asset.decimals}})?$`)
    if (!cleanAmt) {
      nextErrors.amount = '请输入提币金额'
    } else if (!amountPattern.test(cleanAmt) || Number(cleanAmt) <= 0) {
      nextErrors.amount = `请输入大于 0 且最多 ${asset.decimals} 位小数的有效金额`
    }

    setFieldErrors(nextErrors)
    if (Object.keys(nextErrors).length > 0) return null
    return { to_address: cleanAddr, amount: cleanAmt }
  }

  const handleRequestQuote = (e: React.FormEvent) => {
    e.preventDefault()
    if (isUserDisabled) return
    const values = validateForm()
    if (!values) return
    setQuote(undefined)
    quoteMutation.mutate(values)
  }

  const resetAll = () => {
    setCreated(undefined)
    setQuote(undefined)
    setPendingValues(undefined)
    setToAddress('')
    setAmount('')
    setFieldErrors({})
  }

  if (current) {
    const item = current.data
    const itemAmount = current.kind === 'TOKEN' ? current.data.amount : current.data.amount_eth
    const estimatedFee =
      current.kind === 'TOKEN' ? current.data.estimated_gas_eth : current.data.reserved_fee_eth
    const activeStep =
      item.status === 'COMPLETED' ? 3 : item.status === 'FAILED' ? 1 : item.tx_hash ? 2 : 1
    const isCompleted = item.status === 'COMPLETED'
    const isFailed = item.status === 'FAILED'

    return (
      <div className="space-y-5 max-w-4xl">
        <PageHeader title={`${asset.symbol} 提币进度`} subtitle={`提币单号 #${item.id}`} />

        <Panel>
          <div className="p-6 space-y-6">
            {/* Result Hero */}
            <div className="flex flex-col items-center text-center space-y-2 py-2">
              <div
                className={`w-12 h-12 rounded-full flex items-center justify-center ${
                  isCompleted
                    ? 'bg-emerald-50 text-emerald-600 border border-emerald-200'
                    : isFailed
                      ? 'bg-rose-50 text-rose-600 border border-rose-200'
                      : 'bg-blue-50 text-blue-600 border border-blue-200'
                }`}
              >
                {isCompleted ? (
                  <CheckCircle2 className="w-6 h-6" />
                ) : isFailed ? (
                  <AlertOctagon className="w-6 h-6" />
                ) : (
                  <Clock className="w-6 h-6 animate-pulse" />
                )}
              </div>
              <h2 className="text-lg font-bold text-slate-900">
                {isCompleted ? '提币已完成' : isFailed ? '提币失败' : '提币处理中'}
              </h2>
              <p className="text-xs text-slate-500 font-mono">
                {asset.symbol} 提币单 #{item.id}
              </p>
            </div>

            {/* Steps Indicator */}
            <div className="grid grid-cols-4 gap-2 pt-2">
              {STEPS.map((label, idx) => {
                const done = isCompleted || idx < activeStep
                const currentStep = !isCompleted && idx === activeStep
                const errored = isFailed && idx === activeStep
                return (
                  <div key={label} className="flex flex-col items-center gap-1.5 text-center">
                    <div className="flex items-center w-full">
                      <div
                        className={`h-0.5 flex-1 ${
                          idx === 0 ? 'bg-transparent' : done || currentStep ? 'bg-[#09182b]' : 'bg-slate-200'
                        }`}
                      />
                      <div
                        className={`w-6 h-6 rounded-full text-[11px] font-mono font-bold flex items-center justify-center shrink-0 border ${
                          errored
                            ? 'bg-rose-600 text-white border-rose-600'
                            : done
                              ? 'bg-[#09182b] text-white border-[#09182b]'
                              : currentStep
                                ? 'bg-blue-600 text-white border-blue-600'
                                : 'bg-white text-slate-400 border-slate-200'
                        }`}
                      >
                        {done && !errored ? <Check className="w-3.5 h-3.5" /> : idx + 1}
                      </div>
                      <div
                        className={`h-0.5 flex-1 ${
                          idx === STEPS.length - 1
                            ? 'bg-transparent'
                            : idx < activeStep || isCompleted
                              ? 'bg-[#09182b]'
                              : 'bg-slate-200'
                        }`}
                      />
                    </div>
                    <span
                      className={`text-[11px] font-medium ${
                        errored
                          ? 'text-rose-600'
                          : done || currentStep
                            ? 'text-slate-900'
                            : 'text-slate-400'
                      }`}
                    >
                      {label}
                    </span>
                  </div>
                )
              })}
            </div>

            {/* Detail Grid */}
            <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 py-5 border-y border-slate-200 text-xs">
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">状态</div>
                <div>
                  <StatusBadge status={item.status} />
                </div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">提币金额</div>
                <div className="font-mono font-bold text-slate-900 text-sm">
                  {itemAmount} {asset.symbol}
                </div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">
                  {isToken ? '预计平台 Gas' : '预留网络费'}
                </div>
                <div className="font-mono font-semibold text-slate-800">
                  {estimatedFee ? `${estimatedFee} ETH` : '--'}
                </div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">实际网络费</div>
                <div className="font-mono font-semibold text-slate-800">
                  {item.actual_fee_eth ? `${item.actual_fee_eth} ETH` : '--'}
                </div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">确认数</div>
                <div className="font-mono font-semibold text-slate-800">{item.confirmations}</div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">创建时间</div>
                <div className="font-mono text-slate-700">{formatDate(item.created_at)}</div>
              </div>
              <div className="sm:col-span-3 space-y-1">
                <div className="text-[11px] text-slate-400">交易哈希</div>
                <div>
                  <HashLink hash={item.tx_hash} url={item.explorer_url} copyable />
                </div>
              </div>
            </div>

            {item.error_message && <AlertBanner type="error" title={item.error_message} />}

            <div className="flex flex-wrap items-center justify-center gap-3">
              <Button variant="secondary" onClick={resetAll}>
                发起新的提币
              </Button>
              {item.explorer_url && (
                <Button
                  href={item.explorer_url}
                  target="_blank"
                  rel="noreferrer"
                  variant="primary"
                  icon={<ExternalLink className="w-3.5 h-3.5" />}
                >
                  区块浏览器查看
                </Button>
              )}
            </div>
          </div>
        </Panel>
      </div>
    )
  }

  const fee = quote?.kind === 'ETH' ? quote.data.reserved_fee_eth : quote?.data.estimated_gas_eth
  const gasLimit = quote?.data.gas_limit

  return (
    <div className="space-y-5">
      <PageHeader title={`提币 ${asset.symbol}`} subtitle="输入目标地址与金额，先试算网络费再确认广播" />

      {isUserDisabled && (
        <AlertBanner
          type="error"
          title={`用户「${currentUser?.display_name}」已被禁用（DISABLED），无法发起提币。`}
          description="请前往「账户」页面将其状态恢复为 ACTIVE 后再操作。"
        />
      )}

      <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1.35fr)_minmax(320px,0.75fr)] gap-5 items-start">
        <Panel>
          <SectionHeader title="填写提币信息" />
          <QueryState
            loading={balancesQuery.isLoading}
            error={balancesQuery.error}
            retry={() => balancesQuery.refetch()}
          />

          <form onSubmit={handleRequestQuote} className="p-5 space-y-4 text-xs" noValidate>
            {selectedBalance && (
              <div className="flex items-center justify-between px-3.5 py-2.5 rounded-md border-l-3 border-[#09182b] bg-slate-50">
                <span className="text-slate-500">当前可用余额</span>
                <strong className="font-mono text-sm font-bold text-slate-900">
                  {formatETH(selectedBalance.available)} {asset.symbol}
                </strong>
              </div>
            )}

            <div className="space-y-1.5">
              <label htmlFor="withdraw-to-address" className="block font-medium text-slate-700">
                到账地址
              </label>
              <input
                id="withdraw-to-address"
                type="text"
                autoComplete="off"
                disabled={isUserDisabled}
                placeholder="0x..."
                value={toAddress}
                onChange={(e) => {
                  setToAddress(e.target.value)
                  setQuote(undefined)
                  setPendingValues(undefined)
                  if (fieldErrors.to_address) setFieldErrors((p) => ({ ...p, to_address: undefined }))
                }}
                className={`w-full h-10 px-3 rounded-md border font-mono text-xs bg-white transition-colors focus:outline-none focus:ring-1 disabled:bg-slate-50 disabled:text-slate-400 ${
                  fieldErrors.to_address
                    ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                    : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
                }`}
              />
              {fieldErrors.to_address && (
                <p role="alert" className="text-[11px] text-rose-600 font-medium">
                  {fieldErrors.to_address}
                </p>
              )}
            </div>

            <div className="space-y-1.5">
              <div className="flex items-center justify-between">
                <label htmlFor="withdraw-amount" className="block font-medium text-slate-700">
                  提币金额
                </label>
                <span className="text-[11px] text-slate-400 font-mono">最多 {asset.decimals} 位小数</span>
              </div>
              <div className="relative">
                <input
                  id="withdraw-amount"
                  type="text"
                  inputMode="decimal"
                  autoComplete="off"
                  disabled={isUserDisabled}
                  placeholder={isToken ? '10' : '0.001'}
                  value={amount}
                  onChange={(e) => {
                    setAmount(e.target.value)
                    setQuote(undefined)
                    setPendingValues(undefined)
                    if (fieldErrors.amount) setFieldErrors((p) => ({ ...p, amount: undefined }))
                  }}
                  className={`w-full h-10 pl-3 pr-16 rounded-md border font-mono text-xs bg-white transition-colors focus:outline-none focus:ring-1 disabled:bg-slate-50 disabled:text-slate-400 ${
                    fieldErrors.amount
                      ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                      : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
                  }`}
                />
                <span className="absolute right-3 top-1/2 -translate-y-1/2 font-mono text-xs font-semibold text-slate-400 select-none">
                  {asset.symbol}
                </span>
              </div>
              {fieldErrors.amount && (
                <p role="alert" className="text-[11px] text-rose-600 font-medium">
                  {fieldErrors.amount}
                </p>
              )}
            </div>

            {quoteMutation.isError && (
              <AlertBanner type="error" title={quoteMutation.error.message} />
            )}

            <Button
              type="submit"
              variant="primary"
              size="lg"
              block
              disabled={isUserDisabled}
              loading={quoteMutation.isPending}
              icon={<Calculator className="w-4 h-4" />}
            >
              {isUserDisabled ? '用户已禁用，无法提币' : '试算网络费'}
            </Button>
          </form>
        </Panel>

        <Panel>
          <SectionHeader title="费用预览" />
          <div className="p-5 text-xs">
            {quote && pendingValues ? (
              <div className="space-y-4">
                <div className="divide-y divide-slate-100 border-b border-slate-100">
                  <div className="py-2.5 flex items-center justify-between gap-3">
                    <span className="text-slate-500">提币金额</span>
                    <strong className="font-mono text-slate-900">
                      {pendingValues.amount} {asset.symbol}
                    </strong>
                  </div>
                  <div className="py-2.5 flex items-center justify-between gap-3">
                    <span className="text-slate-500">Gas Limit</span>
                    <strong className="font-mono text-slate-900">{gasLimit?.toLocaleString()}</strong>
                  </div>
                  <div className="py-2.5 flex items-center justify-between gap-3">
                    <span className="text-slate-500">
                      {isToken ? '平台预计 Gas' : '最大网络费'}
                    </span>
                    <strong className="font-mono text-slate-900">{fee} ETH</strong>
                  </div>
                </div>

                <AlertBanner
                  type="info"
                  title={
                    isToken
                      ? 'ERC-20 提币 Gas 由平台热钱包支付，不从用户 Token 余额中扣除。'
                      : '实际网络费按链上执行结果结算，未使用的预留费用会返还余额。'
                  }
                />

                {createMutation.isError && (
                  <AlertBanner type="error" title={createMutation.error.message} />
                )}

                <Button
                  variant="danger"
                  size="lg"
                  block
                  disabled={isUserDisabled}
                  loading={createMutation.isPending}
                  icon={<ArrowUpFromLine className="w-4 h-4" />}
                  onClick={() => setConfirmOpen(true)}
                >
                  {isUserDisabled ? '用户已禁用，无法提交' : '提交提币'}
                </Button>
              </div>
            ) : (
              <div className="min-h-[210px] flex flex-col items-center justify-center text-center gap-2.5 text-slate-400">
                <Calculator className="w-7 h-7 text-slate-300" />
                <span>填写到账地址和金额后点击试算网络费</span>
              </div>
            )}
          </div>
        </Panel>
      </div>

      <Modal
        open={confirmOpen}
        onClose={() => {
          if (!createMutation.isPending) setConfirmOpen(false)
        }}
        title={`确认提交 ${asset.symbol} 提币`}
        subtitle="请核对目标地址与预估网络费，链上广播后不可撤销"
        footer={
          <>
            <Button
              variant="secondary"
              size="sm"
              disabled={createMutation.isPending}
              onClick={() => setConfirmOpen(false)}
            >
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              disabled={isUserDisabled}
              loading={createMutation.isPending}
              onClick={() => {
                if (pendingValues && !isUserDisabled) void createMutation.mutateAsync(pendingValues)
              }}
            >
              确认提币
            </Button>
          </>
        }
      >
        {pendingValues && quote && (
          <div className="divide-y divide-slate-100 border border-slate-200 rounded-lg overflow-hidden">
            <div className="p-3 flex items-start justify-between gap-3">
              <span className="text-slate-500 shrink-0">到账地址</span>
              <div className="flex items-center gap-1 min-w-0 justify-end">
                <strong className="font-mono text-slate-900 break-all text-right">
                  {pendingValues.to_address}
                </strong>
                <CopyButton text={pendingValues.to_address} />
              </div>
            </div>
            <div className="p-3 flex items-center justify-between gap-3">
              <span className="text-slate-500">提币金额</span>
              <strong className="font-mono text-slate-900">
                {pendingValues.amount} {asset.symbol}
              </strong>
            </div>
            <div className="p-3 flex items-center justify-between gap-3">
              <span className="text-slate-500">{isToken ? '平台预计 Gas' : '最大网络费'}</span>
              <strong className="font-mono text-slate-900">{fee} ETH</strong>
            </div>
            {!isToken && fee && (
              <div className="p-3 flex items-center justify-between gap-3 bg-slate-50/80">
                <span className="text-slate-600 font-medium">最大扣减总额</span>
                <strong className="font-mono text-slate-900">
                  {addETH(pendingValues.amount, fee)} ETH
                </strong>
              </div>
            )}
          </div>
        )}
      </Modal>
    </div>
  )
}
