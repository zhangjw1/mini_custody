import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  ArrowDownToLine,
  ArrowRightLeft,
  ArrowUpFromLine,
  ChevronLeft,
  ChevronRight,
  Fuel,
  PackageCheck,
  RefreshCw,
} from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import {
  EmptyState,
  formatDate,
  formatETH,
  HashLink,
  PageHeader,
  Panel,
  QueryState,
  StatusBadge,
} from '../components/Common'
import { transactionToDetail, TxDetailDrawer, type TxDetailData } from '../components/TxDetailDrawer'
import { Button } from '../components/ui/Button'
import { TableSkeleton } from '../components/ui/Skeleton'
import type { Transaction } from '../types'

const typeOptions = [
  { value: '', label: '全部类型' },
  { value: 'DEPOSIT', label: 'ETH 充值' },
  { value: 'WITHDRAWAL', label: 'ETH 提币' },
  { value: 'TOKEN_DEPOSIT', label: 'Token 充值' },
  { value: 'TOKEN_WITHDRAWAL', label: 'Token 提币' },
  { value: 'TOKEN_SWEEP', label: 'Token 归集' },
  { value: 'GAS_TOPUP', label: 'Gas 补充' },
]

function TransactionType({ type }: { type: Transaction['type'] }) {
  if (type === 'DEPOSIT' || type === 'TOKEN_DEPOSIT') {
    return (
      <span className="inline-flex items-center gap-1.5 font-semibold text-emerald-700">
        <ArrowDownToLine className="w-3.5 h-3.5" />
        充值
      </span>
    )
  }
  if (type === 'WITHDRAWAL' || type === 'TOKEN_WITHDRAWAL') {
    return (
      <span className="inline-flex items-center gap-1.5 font-semibold text-amber-700">
        <ArrowUpFromLine className="w-3.5 h-3.5" />
        提币
      </span>
    )
  }
  if (type === 'TOKEN_SWEEP') {
    return (
      <span className="inline-flex items-center gap-1.5 font-semibold text-slate-600">
        <PackageCheck className="w-3.5 h-3.5" />
        归集
      </span>
    )
  }
  if (type === 'GAS_TOPUP') {
    return (
      <span className="inline-flex items-center gap-1.5 font-semibold text-slate-600">
        <Fuel className="w-3.5 h-3.5" />
        补 Gas
      </span>
    )
  }
  return (
    <span className="inline-flex items-center gap-1.5 font-semibold text-slate-600">
      <ArrowRightLeft className="w-3.5 h-3.5" />
      {type}
    </span>
  )
}

export default function TransactionsPage() {
  const { assets, asset } = useAppContext()
  const [assetFilter, setAssetFilter] = useState(asset.symbol)
  const [typeFilter, setTypeFilter] = useState('')
  const [page, setPage] = useState(1)
  const [selectedDetail, setSelectedDetail] = useState<TxDetailData | null>(null)
  const pageSize = 20

  useEffect(() => {
    setAssetFilter(asset.symbol)
    setPage(1)
  }, [asset.symbol])

  const query = useQuery({
    queryKey: ['transactions', page, pageSize, assetFilter, typeFilter],
    queryFn: () => api.transactions(page, pageSize, assetFilter, typeFilter),
    refetchInterval: 10_000,
  })

  const items = query.data?.items ?? []
  const hasMore = query.data?.has_more ?? false

  return (
    <div className="space-y-5">
      <PageHeader
        title="资金流水"
        subtitle="点击任意流水行可展开右侧详情抽屉"
        extra={
          <div className="flex items-center gap-2 flex-wrap">
            <select
              aria-label="按资产筛选"
              value={assetFilter}
              onChange={(e) => {
                setAssetFilter(e.target.value)
                setPage(1)
              }}
              className="h-8 px-2.5 rounded-md border border-slate-200 bg-white text-xs font-medium text-slate-700 hover:border-slate-300 focus:outline-none"
            >
              <option value="">全部资产</option>
              {assets.map((item) => (
                <option key={item.id || item.symbol} value={item.symbol}>
                  {item.symbol}
                </option>
              ))}
            </select>

            <select
              aria-label="按类型筛选"
              value={typeFilter}
              onChange={(e) => {
                setTypeFilter(e.target.value)
                setPage(1)
              }}
              className="h-8 px-2.5 rounded-md border border-slate-200 bg-white text-xs font-medium text-slate-700 hover:border-slate-300 focus:outline-none"
            >
              {typeOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>

            <Button
              variant="secondary"
              size="sm"
              loading={query.isFetching}
              icon={<RefreshCw className="w-3.5 h-3.5" />}
              onClick={() => void query.refetch()}
            >
              刷新
            </Button>
          </div>
        }
      />

      <Panel>
        {query.isLoading ? (
          <TableSkeleton rows={8} cols={7} />
        ) : query.error ? (
          <QueryState loading={false} error={query.error} retry={() => query.refetch()} />
        ) : items.length > 0 ? (
          <>
            <div className="overflow-x-auto">
              <table className="w-full text-left border-collapse text-xs">
                <thead>
                  <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                    <th className="py-2.5 px-4">类型</th>
                    <th className="py-2.5 px-4">用户 ID</th>
                    <th className="py-2.5 px-4">资产</th>
                    <th className="py-2.5 px-4 text-right">金额</th>
                    <th className="py-2.5 px-4">交易哈希</th>
                    <th className="py-2.5 px-4 text-right">区块</th>
                    <th className="py-2.5 px-4 text-right">确认数</th>
                    <th className="py-2.5 px-4">状态</th>
                    <th className="py-2.5 px-4 text-right">时间</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-100">
                  {items.map((row) => {
                    const isInbound = ['DEPOSIT', 'TOKEN_DEPOSIT'].includes(row.type)
                    return (
                      <tr
                        key={`${row.type}-${row.id}`}
                        onClick={() => setSelectedDetail(transactionToDetail(row))}
                        className="hover:bg-slate-50/80 transition-colors cursor-pointer"
                      >
                        <td className="py-3 px-4 whitespace-nowrap">
                          <TransactionType type={row.type} />
                        </td>
                        <td className="py-3 px-4 font-mono text-slate-600 whitespace-nowrap">
                          {row.user_id || '--'}
                        </td>
                        <td className="py-3 px-4 whitespace-nowrap">
                          <span className="px-1.5 py-0.5 rounded bg-slate-100 text-slate-700 font-mono text-[11px] font-semibold border border-slate-200">
                            {row.asset}
                          </span>
                        </td>
                        <td className="py-3 px-4 text-right whitespace-nowrap font-mono font-semibold">
                          <span className={isInbound ? 'text-emerald-600' : 'text-slate-900'}>
                            {isInbound ? '+' : '-'}
                            {formatETH(row.amount)} {row.asset}
                          </span>
                        </td>
                        <td className="py-3 px-4 whitespace-nowrap">
                          <HashLink hash={row.tx_hash} url={row.explorer_url} />
                        </td>
                        <td className="py-3 px-4 text-right font-mono text-slate-600 whitespace-nowrap">
                          {row.block_number !== undefined ? row.block_number.toLocaleString() : '--'}
                        </td>
                        <td className="py-3 px-4 text-right font-mono text-slate-600 whitespace-nowrap">
                          {row.confirmations}
                        </td>
                        <td className="py-3 px-4 whitespace-nowrap">
                          <StatusBadge status={row.status} />
                        </td>
                        <td className="py-3 px-4 text-right font-mono text-slate-500 whitespace-nowrap">
                          {formatDate(row.created_at)}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>

            <div className="px-4 py-3 border-t border-slate-100 flex items-center justify-between text-xs text-slate-500">
              <span className="font-mono">
                第 {page} 页 · 当前显示 {items.length} 条
              </span>
              <div className="flex items-center gap-2">
                <Button
                  variant="secondary"
                  size="xs"
                  disabled={page <= 1}
                  icon={<ChevronLeft className="w-3.5 h-3.5" />}
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                >
                  上一页
                </Button>
                <Button
                  variant="secondary"
                  size="xs"
                  disabled={!hasMore}
                  onClick={() => setPage((p) => p + 1)}
                >
                  下一页
                  <ChevronRight className="w-3.5 h-3.5" />
                </Button>
              </div>
            </div>
          </>
        ) : (
          <EmptyState description="当前筛选条件下没有流水" />
        )}
      </Panel>

      <TxDetailDrawer detail={selectedDetail} onClose={() => setSelectedDetail(null)} />
    </div>
  )
}
