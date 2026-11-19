import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertOctagon, ArrowUpFromLine, CheckCircle2, Clock, ShieldCheck } from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import {
  AlertBanner,
  CopyButton,
  HashLink,
  PageHeader,
  Panel,
  SectionHeader,
  StatusBadge,
} from '../components/Common'
import { Button } from '../components/ui/Button'
import { Modal } from '../components/ui/Modal'
import { useToast } from '../components/ui/Toast'
import type { BitcoinWithdrawal } from '../types'

type FormValue = { to_address: string; amount_sats: number }

const BTC_P2WPKH_REGEX = /^tb1q[023456789acdefghjklmnpqrstuvwxyz]{38}$/

export default function BitcoinWithdrawPanel({ userId }: { userId: number }) {
  const { asset, currentUser } = useAppContext()
  const queryClient = useQueryClient()
  const toast = useToast()

  const [toAddress, setToAddress] = useState('')
  const [amountInput, setAmountInput] = useState('')
  const [errors, setErrors] = useState<{ to_address?: string; amount_sats?: string }>({})
  const [pendingSubmit, setPendingSubmit] = useState<FormValue | null>(null)
  const [created, setCreated] = useState<BitcoinWithdrawal>()

  const networkLabel = asset.network === 'bitcoin-testnet4' ? 'Testnet4' : 'Signet'
  const isUserDisabled = currentUser?.status === 'DISABLED'

  useEffect(() => {
    setToAddress('')
    setAmountInput('')
    setErrors({})
    setPendingSubmit(null)
    setCreated(undefined)
  }, [asset.network, userId])

  const detail = useQuery({
    queryKey: ['btc-withdrawal', created?.id],
    queryFn: () => api.bitcoinWithdrawal(created!.id),
    enabled: Boolean(created?.id),
    refetchInterval: (query) =>
      ['COMPLETED', 'FAILED'].includes(query.state.data?.status ?? '') ? false : 3_000,
  })

  const current = detail.data ?? created

  useEffect(() => {
    if (detail.data && ['COMPLETED', 'FAILED'].includes(detail.data.status)) {
      queryClient.invalidateQueries({ queryKey: ['deposits'] })
      queryClient.invalidateQueries({ queryKey: ['bitcoin-sweeps'] })
    }
  }, [detail.data, queryClient])

  const mutation = useMutation({
    mutationFn: (value: FormValue) =>
      api.createBitcoinWithdrawal(userId, crypto.randomUUID(), value),
    onSuccess: (data) => {
      setPendingSubmit(null)
      setCreated(data)
      toast.success(`BTC 提币单 #${data.id} 已创建`)
    },
  })

  const validate = (): FormValue | null => {
    const cleanAddr = toAddress.trim()
    const cleanAmt = amountInput.trim()
    const nextErrors: { to_address?: string; amount_sats?: string } = {}

    if (!cleanAddr) {
      nextErrors.to_address = `请输入 Bitcoin ${networkLabel} 接收地址`
    } else if (!BTC_P2WPKH_REGEX.test(cleanAddr)) {
      nextErrors.to_address = `请输入有效的 ${networkLabel} P2WPKH 地址（小写 tb1q 开头，共 42 位 Bech32 字符）`
    }

    if (!cleanAmt) {
      nextErrors.amount_sats = '请输入提币金额（单位 sat）'
    } else if (!/^\d+$/.test(cleanAmt)) {
      nextErrors.amount_sats = '提币金额必须为正整数（satoshi，不可包含小数）'
    } else {
      const sats = Number(cleanAmt)
      if (!Number.isSafeInteger(sats) || sats < 1) {
        nextErrors.amount_sats = '提币金额至少为 1 sat'
      } else if (sats < 546) {
        nextErrors.amount_sats = '金额低于 Bitcoin Dust Limit（546 sat），可能无法被网络广播'
      }
    }

    setErrors(nextErrors)
    if (Object.keys(nextErrors).length > 0) return null
    return { to_address: cleanAddr, amount_sats: Number(cleanAmt) }
  }

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (isUserDisabled) return
    const valid = validate()
    if (!valid) return
    setPendingSubmit(valid)
  }

  const resetForm = () => {
    setCreated(undefined)
    setPendingSubmit(null)
    setToAddress('')
    setAmountInput('')
    setErrors({})
  }

  if (current) {
    const isCompleted = current.status === 'COMPLETED'
    const isFailed = current.status === 'FAILED'

    return (
      <div className="space-y-5 max-w-4xl">
        <PageHeader title="BTC 提币进度" subtitle={`Bitcoin ${networkLabel} · 提币单 #${current.id}`} />

        <Panel>
          <div className="p-6 space-y-6">
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
                {isCompleted ? 'BTC 提币已完成' : isFailed ? 'BTC 提币失败' : 'BTC 提币处理中'}
              </h2>
              <p className="text-xs text-slate-500 font-mono">BTC 提币单 #{current.id}</p>
            </div>

            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 py-5 border-y border-slate-200 text-xs">
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">状态</div>
                <div>
                  <StatusBadge status={current.status} />
                </div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">提币金额</div>
                <div className="font-mono font-bold text-slate-900">
                  {current.amount_sats.toLocaleString()} sat
                </div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">费率</div>
                <div className="font-mono font-semibold text-slate-800">
                  {current.fee_rate_sat_vb} sat/vB
                </div>
              </div>
              <div className="space-y-1">
                <div className="text-[11px] text-slate-400">交易 ID</div>
                <div>
                  <HashLink hash={current.txid} copyable />
                </div>
              </div>
              <div className="sm:col-span-2 lg:col-span-4 space-y-1">
                <div className="text-[11px] text-slate-400">目标地址</div>
                <div className="flex items-center gap-1.5">
                  <span className="font-mono font-semibold text-slate-900 break-all">
                    {current.to_address}
                  </span>
                  <CopyButton text={current.to_address} />
                </div>
              </div>
            </div>

            <div className="flex justify-center">
              <Button variant="secondary" onClick={resetForm}>
                发起新的提币
              </Button>
            </div>
          </div>
        </Panel>
      </div>
    )
  }

  const parsedSats = /^\d+$/.test(amountInput.trim()) ? Number(amountInput.trim()) : 0
  const approxBtc = parsedSats > 0 ? (parsedSats / 1e8).toFixed(8) : '0.00000000'

  return (
    <div className="space-y-5">
      <PageHeader
        title="提币 BTC"
        subtitle={`Bitcoin ${networkLabel} 原生隔离见证 (P2WPKH) 出金通道`}
      />

      {isUserDisabled && (
        <AlertBanner
          type="error"
          title={`用户「${currentUser?.display_name}」已被禁用（DISABLED），无法发起 BTC 提币。`}
          description="请前往「账户」页面将其状态恢复为 ACTIVE 后再操作。"
        />
      )}

      <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1.35fr)_minmax(320px,0.75fr)] gap-5 items-start">
        <Panel>
          <SectionHeader title={`Bitcoin ${networkLabel} 提币`} />
          <form onSubmit={handleSubmit} className="p-5 space-y-4 text-xs" noValidate>
            <div className="space-y-1.5">
              <div className="flex items-center justify-between">
                <label htmlFor="btc-to-address" className="block font-medium text-slate-700">
                  {networkLabel} 接收地址 (P2WPKH)
                </label>
                <span className="text-[11px] text-slate-400 font-mono">tb1q... (42 chars)</span>
              </div>
              <input
                id="btc-to-address"
                type="text"
                autoComplete="off"
                disabled={isUserDisabled}
                placeholder="tb1q..."
                value={toAddress}
                onChange={(e) => {
                  setToAddress(e.target.value)
                  if (errors.to_address) setErrors((p) => ({ ...p, to_address: undefined }))
                }}
                className={`w-full h-10 px-3 rounded-md border font-mono text-xs bg-white transition-colors focus:outline-none focus:ring-1 disabled:bg-slate-50 disabled:text-slate-400 ${
                  errors.to_address
                    ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                    : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
                }`}
              />
              {errors.to_address && (
                <p role="alert" className="text-[11px] text-rose-600 font-medium">
                  {errors.to_address}
                </p>
              )}
            </div>

            <div className="space-y-1.5">
              <div className="flex items-center justify-between">
                <label htmlFor="btc-amount-sats" className="block font-medium text-slate-700">
                  提币金额 (satoshi)
                </label>
                <span className="text-[11px] text-slate-500 font-mono">≈ {approxBtc} BTC</span>
              </div>
              <div className="relative">
                <input
                  id="btc-amount-sats"
                  type="text"
                  inputMode="numeric"
                  autoComplete="off"
                  disabled={isUserDisabled}
                  placeholder="例如 10000"
                  value={amountInput}
                  onChange={(e) => {
                    setAmountInput(e.target.value)
                    if (errors.amount_sats) setErrors((p) => ({ ...p, amount_sats: undefined }))
                  }}
                  className={`w-full h-10 pl-3 pr-14 rounded-md border font-mono text-xs bg-white transition-colors focus:outline-none focus:ring-1 disabled:bg-slate-50 disabled:text-slate-400 ${
                    errors.amount_sats
                      ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                      : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
                  }`}
                />
                <span className="absolute right-3 top-1/2 -translate-y-1/2 font-mono text-xs font-semibold text-slate-400 select-none">
                  sat
                </span>
              </div>
              {errors.amount_sats && (
                <p role="alert" className="text-[11px] text-rose-600 font-medium">
                  {errors.amount_sats}
                </p>
              )}
            </div>

            {mutation.isError && <AlertBanner type="error" title={mutation.error.message} />}

            <Button
              type="submit"
              variant="danger"
              size="lg"
              block
              disabled={isUserDisabled}
              loading={mutation.isPending}
              icon={<ArrowUpFromLine className="w-4 h-4" />}
            >
              {isUserDisabled ? '用户已禁用，无法提币' : '提交 BTC 提币'}
            </Button>
          </form>
        </Panel>

        <Panel>
          <SectionHeader title="出金规则与安全校验" />
          <div className="p-5 space-y-3 text-xs">
            <div className="divide-y divide-slate-100 border-b border-slate-100">
              <div className="py-2.5 flex items-center justify-between gap-3">
                <span className="text-slate-500">目标网络</span>
                <strong className="font-semibold text-slate-900">Bitcoin {networkLabel}</strong>
              </div>
              <div className="py-2.5 flex items-center justify-between gap-3">
                <span className="text-slate-500">地址格式</span>
                <strong className="font-mono text-slate-900">Bech32 P2WPKH (tb1q)</strong>
              </div>
              <div className="py-2.5 flex items-center justify-between gap-3">
                <span className="text-slate-500">最小输出阈值</span>
                <strong className="font-mono text-slate-900">546 sat (Dust Limit)</strong>
              </div>
            </div>

            <AlertBanner
              type="info"
              title="提交后系统将自动选择 UTXO、估算 sat/vB 费率并完成 PSBT 签名广播。"
            />
            <div className="flex items-center gap-2 text-[11px] text-slate-500 pt-1">
              <ShieldCheck className="w-4 h-4 text-emerald-600 shrink-0" />
              <span>每次提交自动携带随机 Idempotency-Key 防止重复广播</span>
            </div>
          </div>
        </Panel>
      </div>

      <Modal
        open={Boolean(pendingSubmit)}
        onClose={() => {
          if (!mutation.isPending) setPendingSubmit(null)
        }}
        title={`确认提交 BTC (${networkLabel}) 提币`}
        subtitle="请仔细核对 Bech32 地址与 satoshi 金额"
        footer={
          <>
            <Button
              variant="secondary"
              size="sm"
              disabled={mutation.isPending}
              onClick={() => setPendingSubmit(null)}
            >
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              disabled={isUserDisabled}
              loading={mutation.isPending}
              onClick={() => {
                if (pendingSubmit && !isUserDisabled) mutation.mutate(pendingSubmit)
              }}
            >
              确认提交
            </Button>
          </>
        }
      >
        {pendingSubmit && (
          <div className="divide-y divide-slate-100 border border-slate-200 rounded-lg overflow-hidden">
            <div className="p-3 flex items-start justify-between gap-3">
              <span className="text-slate-500 shrink-0">接收地址</span>
              <div className="flex items-center gap-1 min-w-0 justify-end">
                <strong className="font-mono text-slate-900 break-all text-right">
                  {pendingSubmit.to_address}
                </strong>
                <CopyButton text={pendingSubmit.to_address} />
              </div>
            </div>
            <div className="p-3 flex items-center justify-between gap-3">
              <span className="text-slate-500">提币金额</span>
              <strong className="font-mono text-slate-900">
                {pendingSubmit.amount_sats.toLocaleString()} sat (≈{' '}
                {(pendingSubmit.amount_sats / 1e8).toFixed(8)} BTC)
              </strong>
            </div>
          </div>
        )}
      </Modal>
    </div>
  )
}
