import type { ChainStatus } from '../types'
import { formatDate } from './Common'

interface FooterBarProps {
  chain?: ChainStatus
  isLoading?: boolean
  isError?: boolean
}

export function FooterBar({ chain, isLoading, isError }: FooterBarProps) {
  const status = chain?.status ?? (isError ? 'DOWN' : isLoading ? 'CHECKING' : 'UNKNOWN')
  const statusColor =
    status === 'HEALTHY'
      ? 'text-emerald-600'
      : status === 'DEGRADED'
        ? 'text-amber-600'
        : status === 'DOWN'
          ? 'text-rose-600'
          : 'text-blue-600'

  const statusText =
    status === 'HEALTHY'
      ? '链上节点正常'
      : status === 'DEGRADED'
        ? '节点降级 (Fallback)'
        : status === 'DOWN'
          ? '节点连接异常'
          : '节点检查中'

  const endpointDisplay = chain?.endpoint?.trim() || '--'
  const isFallback = Boolean(chain?.last_error) || status === 'DEGRADED'

  return (
    <footer className="h-9 bg-white border-t border-slate-200 px-4 md:px-5 flex items-center justify-between gap-4 text-[11px] text-slate-500 shrink-0 select-none overflow-x-auto font-mono">
      <div className="flex items-center gap-4 min-w-0">
        <span className={`inline-flex items-center gap-1.5 font-semibold shrink-0 ${statusColor}`}>
          <span
            className={`w-1.5 h-1.5 rounded-full ${
              status === 'HEALTHY'
                ? 'bg-emerald-500'
                : status === 'DEGRADED'
                  ? 'bg-amber-500'
                  : status === 'DOWN'
                    ? 'bg-rose-500'
                    : 'bg-blue-500 animate-pulse'
            }`}
          />
          {statusText}
        </span>

        <span className="text-slate-300">|</span>

        <span className="flex items-center gap-1.5 min-w-0 max-w-[320px]" title={`RPC Endpoint: ${endpointDisplay}`}>
          <span className="text-slate-400 shrink-0">
            RPC ({isFallback ? 'fallback' : 'primary'}):
          </span>
          <span className="text-slate-700 truncate">{endpointDisplay}</span>
        </span>
      </div>

      <div className="flex items-center gap-4 shrink-0">
        <span>
          scan_height:{' '}
          <strong className="text-slate-700 font-semibold">
            {chain ? chain.scan_height.toLocaleString() : '--'}
          </strong>
        </span>
        <span>
          network_height:{' '}
          <strong className="text-slate-700 font-semibold">
            {chain ? chain.network_height.toLocaleString() : '--'}
          </strong>
        </span>
        <span className="hidden sm:inline">
          lag:{' '}
          <strong className={chain && chain.lag > 5 ? 'text-amber-600 font-semibold' : 'text-slate-700 font-semibold'}>
            {chain ? chain.lag : '--'}
          </strong>
        </span>
        <span className="text-slate-400 hidden md:inline">
          检查时间: {chain?.checked_at ? formatDate(chain.checked_at) : '--'}
        </span>
      </div>
    </footer>
  )
}
