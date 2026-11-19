import { AlertOctagon, CheckCircle2, Clock, ExternalLink } from 'lucide-react'
import type { Transaction } from '../types'
import { CopyButton, formatDate, formatETH } from './Common'
import { Button } from './ui/Button'
import { Drawer } from './ui/Drawer'
import { StatusBadge } from './ui/StatusBadge'

export type DetailField = {
  label: string
  value: string
  mono?: boolean
  copyable?: boolean
  href?: string
}

export type TxDetailData = {
  title: string
  subtitle?: string
  status: string
  amountDisplay?: string
  isInbound?: boolean
  txHash?: string
  explorerUrl?: string
  blockNumber?: number
  blockUrl?: string
  confirmations?: number
  errorMessage?: string
  fields: DetailField[]
}

const TX_TYPE_LABELS: Record<Transaction['type'], string> = {
  DEPOSIT: 'ETH 充值',
  WITHDRAWAL: 'ETH 提币',
  TOKEN_DEPOSIT: 'Token 充值',
  TOKEN_WITHDRAWAL: 'Token 提币',
  TOKEN_SWEEP: 'Token 归集',
  GAS_TOPUP: 'Gas 补充',
}

export function transactionToDetail(tx: Transaction): TxDetailData {
  const isInbound = tx.type === 'DEPOSIT' || tx.type === 'TOKEN_DEPOSIT'
  const sign = isInbound ? '+' : '-'

  return {
    title: `${TX_TYPE_LABELS[tx.type] ?? tx.type}详情`,
    subtitle: `${tx.type} #${tx.id}`,
    status: tx.status,
    amountDisplay: `${sign}${formatETH(tx.amount)} ${tx.asset}`,
    isInbound,
    txHash: tx.tx_hash,
    explorerUrl: tx.explorer_url,
    blockNumber: tx.block_number,
    confirmations: tx.confirmations,
    fields: [
      { label: '记录类型', value: TX_TYPE_LABELS[tx.type] ?? tx.type },
      { label: '流水 ID', value: `#${tx.id}`, mono: true },
      { label: '用户 ID', value: tx.user_id ? String(tx.user_id) : '--', mono: true },
      { label: '资产符号', value: `${tx.asset} (decimals=${tx.decimals})`, mono: true },
      { label: '格式化金额', value: `${sign}${tx.amount} ${tx.asset}`, mono: true },
      { label: '原始最小单位', value: tx.amount_units || tx.amount_wei || '--', mono: true, copyable: Boolean(tx.amount_units || tx.amount_wei) },
      { label: '区块高度', value: tx.block_number !== undefined ? `#${tx.block_number.toLocaleString()}` : '--', mono: true },
      { label: '当前确认数', value: String(tx.confirmations ?? 0), mono: true },
      { label: '创建时间', value: formatDate(tx.created_at), mono: true },
      { label: '更新时间', value: formatDate(tx.updated_at), mono: true },
    ],
  }
}

export function TxDetailDrawer({
  detail,
  onClose,
}: {
  detail: TxDetailData | null
  onClose: () => void
}) {
  if (!detail) return null

  const normalized = detail.status.toUpperCase()
  const isSuccess = ['CREDITED', 'COMPLETED', 'CONFIRMED', 'FINALIZED', 'HEALTHY'].includes(normalized)
  const isFailed = ['FAILED', 'DOWN', 'REORGED', 'ERROR'].includes(normalized)
  const isProcessing = ['CONFIRMING', 'BROADCASTED', 'BROADCASTING', 'SIGNING', 'SIGNED'].includes(normalized)

  return (
    <Drawer
      open={Boolean(detail)}
      onClose={onClose}
      title={detail.title}
      subtitle={detail.subtitle}
      badge={<StatusBadge status={detail.status} />}
      footer={
        <>
          {detail.explorerUrl && (
            <Button
              href={detail.explorerUrl}
              target="_blank"
              rel="noreferrer"
              variant="secondary"
              size="sm"
              icon={<ExternalLink className="w-3.5 h-3.5" />}
            >
              前往区块浏览器
            </Button>
          )}
          <Button variant="primary" size="sm" onClick={onClose}>
            关闭详情
          </Button>
        </>
      }
    >
      <div className="space-y-4">
        {isFailed ? (
          <div className="bg-rose-50 border border-rose-200 rounded-lg p-3 text-rose-800 flex items-start gap-2.5">
            <AlertOctagon className="w-4 h-4 text-rose-600 shrink-0 mt-0.5" />
            <div>
              <div className="font-bold">交易状态异常 ({detail.status})</div>
              <div className="text-[11px] text-rose-700 mt-0.5 break-all">
                {detail.errorMessage || '该笔记录执行失败或已被回滚，请核对链上状态与 Worker 日志。'}
              </div>
            </div>
          </div>
        ) : isProcessing ? (
          <div className="bg-blue-50 border border-blue-200 rounded-lg p-3 text-blue-900 flex items-start gap-2.5">
            <Clock className="w-4 h-4 text-blue-600 shrink-0 mt-0.5" />
            <div>
              <div className="font-bold">链上处理中 · {detail.status}</div>
              <div className="text-[11px] text-blue-700 mt-0.5">
                当前确认数：{detail.confirmations ?? 0}，系统正在持续追踪区块确认状态。
              </div>
            </div>
          </div>
        ) : isSuccess ? (
          <div className="bg-emerald-50 border border-emerald-200 rounded-lg p-3 text-emerald-900 flex items-center gap-2">
            <CheckCircle2 className="w-4 h-4 text-emerald-600 shrink-0" />
            <span className="font-bold">状态已确认 ({detail.status})</span>
          </div>
        ) : (
          <div className="bg-slate-50 border border-slate-200 rounded-lg p-3 text-slate-700 flex items-center gap-2">
            <Clock className="w-4 h-4 text-slate-500 shrink-0" />
            <span className="font-semibold">当前状态：{detail.status}</span>
          </div>
        )}

        {detail.amountDisplay && (
          <div className="rounded-lg border border-slate-200 bg-slate-50/70 px-4 py-3 flex items-center justify-between">
            <span className="text-slate-500">变动金额</span>
            <strong
              className={`font-mono text-base font-bold ${
                detail.isInbound === true
                  ? 'text-emerald-600'
                  : detail.isInbound === false
                    ? 'text-slate-900'
                    : 'text-slate-900'
              }`}
            >
              {detail.amountDisplay}
            </strong>
          </div>
        )}

        <div className="divide-y divide-slate-100 border border-slate-200 rounded-lg overflow-hidden">
          {detail.txHash && (
            <div className="p-3 flex flex-col gap-1 hover:bg-slate-50/70">
              <div className="flex items-center justify-between">
                <span className="text-slate-400">交易哈希 (Tx Hash)</span>
                <div className="flex items-center gap-1">
                  <CopyButton text={detail.txHash} title="复制交易哈希" toastMessage="交易哈希已复制" />
                  {detail.explorerUrl && (
                    <a
                      href={detail.explorerUrl}
                      target="_blank"
                      rel="noreferrer"
                      className="p-1 text-slate-400 hover:text-blue-600 rounded"
                      title="在区块浏览器中查看"
                    >
                      <ExternalLink className="w-3.5 h-3.5" />
                    </a>
                  )}
                </div>
              </div>
              <div className="font-mono text-[11px] text-slate-800 break-all select-all">
                {detail.txHash}
              </div>
            </div>
          )}

          {detail.fields.map((field) => (
            <div
              key={field.label}
              className="px-3 py-2.5 flex items-start justify-between gap-4 hover:bg-slate-50/70"
            >
              <span className="text-slate-400 shrink-0">{field.label}</span>
              <div className="flex items-center gap-1.5 min-w-0 justify-end text-right">
                {field.href ? (
                  <a
                    href={field.href}
                    target="_blank"
                    rel="noreferrer"
                    className={`text-blue-600 hover:underline break-all ${field.mono ? 'font-mono' : ''}`}
                  >
                    {field.value}
                  </a>
                ) : (
                  <span
                    className={`text-slate-800 font-medium break-all ${field.mono ? 'font-mono' : ''}`}
                  >
                    {field.value}
                  </span>
                )}
                {field.copyable && field.value && field.value !== '--' && (
                  <CopyButton text={field.value} title={`复制${field.label}`} />
                )}
              </div>
            </div>
          ))}
        </div>
      </div>
    </Drawer>
  )
}
