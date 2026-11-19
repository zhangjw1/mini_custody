import {
  AlertCircle,
  AlertTriangle,
  ArrowUpRight,
  Check,
  CheckCircle2,
  Copy,
  Info,
  RefreshCw,
} from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { ApiError } from '../api'
import { Button } from './ui/Button'
import { EmptyState as UiEmptyState } from './ui/EmptyState'
import { CardSkeleton } from './ui/Skeleton'
import { StatusBadge } from './ui/StatusBadge'
import { useToast } from './ui/Toast'

export { StatusBadge }

export function StatusTag({ status }: { status: string }) {
  return <StatusBadge status={status} />
}

export function PageHeader({
  title,
  subtitle,
  extra,
}: {
  title: string
  subtitle?: string
  extra?: ReactNode
}) {
  return (
    <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3 mb-5">
      <div className="min-w-0">
        <h1 className="text-xl font-bold text-slate-900 tracking-tight leading-snug">{title}</h1>
        {subtitle && <p className="text-xs text-slate-500 mt-0.5">{subtitle}</p>}
      </div>
      {extra && <div className="flex items-center gap-2 shrink-0 flex-wrap">{extra}</div>}
    </div>
  )
}

export function SectionHeader({
  title,
  subtitle,
  extra,
}: {
  title: string
  subtitle?: string
  extra?: ReactNode
}) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 px-4 py-3 border-b border-slate-100">
      <div className="flex items-center gap-2 min-w-0">
        <h2 className="text-xs font-bold uppercase tracking-wider text-slate-800">{title}</h2>
        {subtitle && <span className="text-xs text-slate-400 font-normal">{subtitle}</span>}
      </div>
      {extra && <div className="flex items-center gap-2 text-xs text-slate-500">{extra}</div>}
    </div>
  )
}

export function Panel({
  children,
  className = '',
}: {
  children: ReactNode
  className?: string
}) {
  return (
    <section className={`bg-white border border-slate-200 rounded-lg shadow-2xs overflow-hidden ${className}`}>
      {children}
    </section>
  )
}

export function QueryState({
  loading,
  error,
  retry,
}: {
  loading: boolean
  error: unknown
  retry?: () => void
}) {
  if (loading) {
    return <CardSkeleton lines={4} />
  }
  if (error) {
    const message = error instanceof ApiError ? error.message : '数据加载失败'
    return (
      <div className="p-5 flex flex-col sm:flex-row items-start sm:items-center justify-between gap-3 bg-amber-50/70 border-y border-amber-100 text-xs text-amber-900">
        <div className="flex items-center gap-2.5">
          <AlertTriangle className="w-4 h-4 text-amber-600 shrink-0" />
          <span className="font-medium">{message}</span>
        </div>
        {retry && (
          <Button variant="secondary" size="xs" icon={<RefreshCw className="w-3 h-3" />} onClick={retry}>
            重新加载
          </Button>
        )}
      </div>
    )
  }
  return null
}

export function EmptyState({ description = '暂无数据', title }: { description?: string; title?: string }) {
  return <UiEmptyState title={title} description={description} />
}

export function AlertBanner({
  type = 'info',
  title,
  description,
  className = '',
}: {
  type?: 'info' | 'warning' | 'error' | 'success'
  title: ReactNode
  description?: ReactNode
  className?: string
}) {
  const styles = {
    info: {
      box: 'bg-blue-50/80 border-blue-200 text-blue-900',
      icon: <Info className="w-4 h-4 text-blue-600 shrink-0 mt-0.5" />,
    },
    warning: {
      box: 'bg-amber-50/80 border-amber-200 text-amber-900',
      icon: <AlertTriangle className="w-4 h-4 text-amber-600 shrink-0 mt-0.5" />,
    },
    error: {
      box: 'bg-rose-50/80 border-rose-200 text-rose-900',
      icon: <AlertCircle className="w-4 h-4 text-rose-600 shrink-0 mt-0.5" />,
    },
    success: {
      box: 'bg-emerald-50/80 border-emerald-200 text-emerald-900',
      icon: <CheckCircle2 className="w-4 h-4 text-emerald-600 shrink-0 mt-0.5" />,
    },
  }[type]

  return (
    <div className={`flex items-start gap-2.5 rounded-lg border px-3.5 py-2.5 text-xs ${styles.box} ${className}`}>
      {styles.icon}
      <div className="min-w-0 leading-relaxed">
        <div className="font-medium">{title}</div>
        {description && <div className="opacity-85 mt-0.5">{description}</div>}
      </div>
    </div>
  )
}

export function CopyButton({
  text,
  label,
  toastMessage = '已复制到剪贴板',
  title = '复制',
  className = '',
}: {
  text: string
  label?: string
  toastMessage?: string
  title?: string
  className?: string
}) {
  const [copied, setCopied] = useState(false)
  const toast = useToast()

  const handleCopy = async (e: React.MouseEvent) => {
    e.stopPropagation()
    if (!text) return
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      toast.success(toastMessage)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      toast.error('复制失败，请手动选择复制')
    }
  }

  if (label) {
    return (
      <button
        type="button"
        onClick={handleCopy}
        title={title}
        className={`inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md border text-[11px] font-medium transition-colors cursor-pointer ${
          copied
            ? 'border-emerald-200 bg-emerald-50 text-emerald-700'
            : 'border-slate-200 bg-white text-slate-600 hover:bg-slate-50 hover:text-slate-900'
        } ${className}`}
      >
        {copied ? <Check className="w-3 h-3 text-emerald-600" /> : <Copy className="w-3 h-3" />}
        <span>{copied ? '已复制' : label}</span>
      </button>
    )
  }

  return (
    <button
      type="button"
      onClick={handleCopy}
      title={copied ? '已复制' : title}
      aria-label={title}
      className={`inline-flex items-center justify-center p-1 rounded text-slate-400 hover:text-slate-800 hover:bg-slate-100 transition-colors cursor-pointer shrink-0 ${className}`}
    >
      {copied ? <Check className="w-3.5 h-3.5 text-emerald-600" /> : <Copy className="w-3.5 h-3.5" />}
    </button>
  )
}

export function AddressText({
  value,
  short = false,
  className = '',
}: {
  value: string
  short?: boolean
  className?: string
}) {
  if (!value) return <span className="text-slate-400 text-xs">--</span>
  const display =
    short && value.length > 16 ? `${value.slice(0, 8)}...${value.slice(-6)}` : value

  return (
    <span className={`inline-flex items-center gap-1 min-w-0 max-w-full ${className}`}>
      <span className="font-mono text-xs text-slate-700 truncate" title={value}>
        {display}
      </span>
      <CopyButton text={value} title="复制地址" toastMessage="地址已复制" />
    </span>
  )
}

export function HashLink({
  hash,
  url,
  copyable = false,
}: {
  hash?: string
  url?: string
  copyable?: boolean
}) {
  if (!hash) return <span className="text-slate-400 text-xs">尚未生成</span>
  const short = hash.length > 20 ? `${hash.slice(0, 10)}...${hash.slice(-8)}` : hash

  return (
    <span className="inline-flex items-center gap-1 whitespace-nowrap">
      {url ? (
        <a
          className="inline-flex items-center gap-1 font-mono text-xs text-slate-600 hover:text-blue-600 hover:underline transition-colors"
          href={url}
          target="_blank"
          rel="noreferrer"
          title={hash}
          onClick={(e) => e.stopPropagation()}
        >
          <span>{short}</span>
          <ArrowUpRight className="w-3.5 h-3.5 shrink-0" />
        </a>
      ) : (
        <span className="font-mono text-xs text-slate-600" title={hash}>
          {short}
        </span>
      )}
      {copyable && <CopyButton text={hash} title="复制哈希" toastMessage="交易哈希已复制" />}
    </span>
  )
}

export function formatDate(value: string): string {
  if (!value) return '--'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(date)
}

export function formatETH(value: string): string {
  if (!value) return '0'
  const [whole, fraction = ''] = value.split('.')
  const visibleFraction = fraction.slice(0, 8).replace(/0+$/, '')
  return visibleFraction ? `${whole}.${visibleFraction}` : whole
}

export function addETH(left: string, right: string): string {
  const scale = 18
  const toWei = (value: string) => {
    const [whole, fraction = ''] = value.split('.')
    return BigInt(whole || '0') * 10n ** 18n + BigInt(fraction.padEnd(scale, '0').slice(0, scale))
  }
  const wei = toWei(left) + toWei(right)
  const whole = wei / 10n ** 18n
  const fraction = (wei % 10n ** 18n).toString().padStart(scale, '0').replace(/0+$/, '')
  return fraction ? `${whole}.${fraction}` : whole.toString()
}
