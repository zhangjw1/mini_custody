import { Button, Empty, Result, Skeleton, Tag, Tooltip } from 'antd'
import { ArrowUpRight, Copy, RefreshCw } from 'lucide-react'
import type { ReactNode } from 'react'
import { ApiError } from '../api'

const statusTone: Record<string, string> = {
  HEALTHY: 'success',
  CREDITED: 'success',
  COMPLETED: 'success',
  CONFIRMED: 'success',
  CONFIRMING: 'processing',
  BROADCASTED: 'processing',
  BROADCASTING: 'processing',
  SIGNING: 'processing',
  SIGNED: 'processing',
  CREATED: 'default',
  DETECTED: 'default',
  DEGRADED: 'warning',
	LOW_BALANCE: 'warning',
  BROADCAST_UNKNOWN: 'warning',
  FAILED: 'error',
  DOWN: 'error',
}

export function StatusTag({ status }: { status: string }) {
  return <Tag color={statusTone[status] ?? 'default'}>{status}</Tag>
}

export function PageHeader({ title, extra }: { title: string; extra?: ReactNode }) {
  return (
    <div className="page-header">
      <h1>{title}</h1>
      {extra}
    </div>
  )
}

export function SectionHeader({ title, extra }: { title: string; extra?: ReactNode }) {
  return (
    <div className="section-header">
      <h2>{title}</h2>
      {extra}
    </div>
  )
}

export function QueryState({ loading, error, retry }: { loading: boolean; error: unknown; retry?: () => void }) {
  if (loading) {
    return <Skeleton active paragraph={{ rows: 5 }} />
  }
  if (error) {
    const message = error instanceof ApiError ? error.message : '数据加载失败'
    return (
      <Result
        status="warning"
        title={message}
        extra={retry ? <Button icon={<RefreshCw size={16} />} onClick={retry}>重新加载</Button> : undefined}
      />
    )
  }
  return null
}

export function EmptyState({ description = '暂无数据' }: { description?: string }) {
  return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={description} />
}

export function AddressText({ value }: { value: string }) {
  const copy = async () => navigator.clipboard.writeText(value)
  return (
    <span className="address-line">
      <span className="mono truncate-value" title={value}>{value}</span>
      <Tooltip title="复制地址">
        <Button type="text" size="small" icon={<Copy size={15} />} onClick={copy} aria-label="复制地址" />
      </Tooltip>
    </span>
  )
}

export function HashLink({ hash, url }: { hash?: string; url?: string }) {
  if (!hash) return <span className="muted">尚未生成</span>
  const short = `${hash.slice(0, 10)}...${hash.slice(-8)}`
  if (!url) return <span className="mono">{short}</span>
  return (
    <a className="hash-link mono" href={url} target="_blank" rel="noreferrer" title={hash}>
      {short}<ArrowUpRight size={14} />
    </a>
  )
}

export function formatDate(value: string): string {
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).format(new Date(value))
}

export function formatETH(value: string): string {
  const [whole, fraction = ''] = value.split('.')
  const visibleFraction = fraction.slice(0, 8).replace(/0+$/, '')
  return visibleFraction ? `${whole}.${visibleFraction}` : whole
}

export function addETH(left: string, right: string): string {
  const scale = 18
  const toWei = (value: string) => {
    const [whole, fraction = ''] = value.split('.')
    return BigInt(whole) * (10n ** 18n) + BigInt(fraction.padEnd(scale, '0').slice(0, scale))
  }
  const wei = toWei(left) + toWei(right)
  const whole = wei / (10n ** 18n)
  const fraction = (wei % (10n ** 18n)).toString().padStart(scale, '0').replace(/0+$/, '')
  return fraction ? `${whole}.${fraction}` : whole.toString()
}
