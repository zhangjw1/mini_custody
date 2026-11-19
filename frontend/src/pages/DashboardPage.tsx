import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import {
  AlertTriangle,
  ArrowDownToLine,
  ArrowRight,
  ArrowUpFromLine,
  Blocks,
  ChevronRight,
  CircleGauge,
  Fuel,
  PackageCheck,
} from 'lucide-react'
import { Link } from 'react-router-dom'
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
  SectionHeader,
  StatusBadge,
} from '../components/Common'
import { transactionToDetail, TxDetailDrawer, type TxDetailData } from '../components/TxDetailDrawer'
import { TableSkeleton } from '../components/ui/Skeleton'
import type { Transaction, WorkerError } from '../types'

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
  return (
    <span className="inline-flex items-center gap-1.5 font-semibold text-slate-600">
      {type === 'TOKEN_SWEEP' ? <PackageCheck className="w-3.5 h-3.5" /> : <Fuel className="w-3.5 h-3.5" />}
      {type === 'TOKEN_SWEEP' ? '归集' : '补 Gas'}
    </span>
  )
}

function workerErrorToDetail(err: WorkerError): TxDetailData {
  return {
    title: 'Worker 异常详情',
    subtitle: `${err.worker} · ${err.stage}`,
    status: 'FAILED',
    errorMessage: err.error_message,
    fields: [
      { label: '异常 ID', value: `#${err.id}`, mono: true },
      { label: 'Worker', value: err.worker, mono: true },
      { label: '阶段', value: err.stage, mono: true },
      { label: '错误码', value: err.error_code, mono: true },
      { label: '关联类型', value: err.reference_type || '--', mono: true },
      { label: '关联 ID', value: err.reference_id !== undefined ? String(err.reference_id) : '--', mono: true },
      { label: '重试次数', value: String(err.retry_count), mono: true },
      { label: '首次发生', value: formatDate(err.first_occurred_at), mono: true },
      { label: '最后发生', value: formatDate(err.last_occurred_at), mono: true },
      { label: '完整错误信息', value: err.error_message, mono: true, copyable: true },
    ],
  }
}

export default function DashboardPage() {
  const { userId, asset } = useAppContext()
  const [selectedDetail, setSelectedDetail] = useState<TxDetailData | null>(null)

  const balancesQuery = useQuery({
    queryKey: ['balances', userId],
    queryFn: () => api.balances(userId),
    enabled: userId > 0,
    refetchInterval: 10_000,
  })
  const chainQuery = useQuery({
    queryKey: ['chain'],
    queryFn: api.chain,
    refetchInterval: 15_000,
  })
  const transactionsQuery = useQuery({
    queryKey: ['transactions', 1, 5, asset.symbol, ''],
    queryFn: () => api.transactions(1, 5, asset.symbol),
    refetchInterval: 10_000,
  })
  const errorsQuery = useQuery({
    queryKey: ['worker-errors', 1, 5],
    queryFn: () => api.workerErrors(1, 5),
    refetchInterval: 15_000,
  })

  const currentBalance = balancesQuery.data?.items.find((item) => item.asset === asset.symbol)

  return (
    <div className="space-y-5">
      <PageHeader
        title="资产总览"
        subtitle={`当前资产 ${asset.symbol} · 每 10 秒自动同步最新余额与流水`}
      />

      {/* High-density Metrics Band */}
      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 bg-white border border-slate-200 rounded-lg divide-y sm:divide-y-0 sm:divide-x divide-slate-200 shadow-2xs overflow-hidden">
        <div className="p-4 flex items-start gap-3">
          <div className="w-8 h-8 rounded-md bg-[#09182b] text-cyan-400 flex items-center justify-center shrink-0">
            <CircleGauge className="w-4 h-4" />
          </div>
          <div className="min-w-0">
            <div className="text-[11px] font-semibold text-slate-500 uppercase tracking-wider">
              {asset.symbol} 可用余额
            </div>
            <div className="mt-1 font-mono text-lg font-bold text-slate-900 truncate">
              {balancesQuery.data ? formatETH(currentBalance?.available ?? '0') : '--'}{' '}
              <span className="text-xs font-medium text-slate-500">{asset.symbol}</span>
            </div>
          </div>
        </div>

        <div className="p-4 flex items-start gap-3">
          <div className="w-8 h-8 rounded-md bg-emerald-50 text-emerald-600 border border-emerald-200 flex items-center justify-center shrink-0">
            <ArrowDownToLine className="w-4 h-4" />
          </div>
          <div className="min-w-0">
            <div className="text-[11px] font-semibold text-slate-500 uppercase tracking-wider">
              待确认充值
            </div>
            <div className="mt-1 font-mono text-lg font-bold text-slate-900 truncate">
              {balancesQuery.data ? formatETH(currentBalance?.pending_deposit ?? '0') : '--'}{' '}
              <span className="text-xs font-medium text-slate-500">{asset.symbol}</span>
            </div>
          </div>
        </div>

        <div className="p-4 flex items-start gap-3">
          <div className="w-8 h-8 rounded-md bg-amber-50 text-amber-600 border border-amber-200 flex items-center justify-center shrink-0">
            <ArrowUpFromLine className="w-4 h-4" />
          </div>
          <div className="min-w-0">
            <div className="text-[11px] font-semibold text-slate-500 uppercase tracking-wider">
              处理中提币
            </div>
            <div className="mt-1 font-mono text-lg font-bold text-slate-900 truncate">
              {balancesQuery.data ? formatETH(currentBalance?.pending_withdrawal ?? '0') : '--'}{' '}
              <span className="text-xs font-medium text-slate-500">{asset.symbol}</span>
            </div>
          </div>
        </div>

        <div className="p-4 flex items-start gap-3">
          <div className="w-8 h-8 rounded-md bg-slate-100 text-slate-700 border border-slate-200 flex items-center justify-center shrink-0">
            <Blocks className="w-4 h-4" />
          </div>
          <div className="min-w-0">
            <div className="text-[11px] font-semibold text-slate-500 uppercase tracking-wider">
              扫描落后区块
            </div>
            <div className="mt-1 font-mono text-lg font-bold text-slate-900 truncate">
              {chainQuery.data?.lag ?? '--'}{' '}
              <span className="text-xs font-medium text-slate-500">blocks</span>
            </div>
          </div>
        </div>
      </div>

      {/* Chain Services Panel */}
      <Panel>
        <SectionHeader
          title="链上服务"
          subtitle="主链扫描、Token 索引与热钱包状态"
          extra={
            chainQuery.data && (
              <span className="font-mono text-[11px] text-slate-400">
                最近检查 {formatDate(chainQuery.data.checked_at)}
              </span>
            )
          }
        />
        <QueryState loading={chainQuery.isLoading} error={chainQuery.error} retry={() => chainQuery.refetch()} />
        {chainQuery.data && (
          <div className="p-4 grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-5 gap-4 text-xs">
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">运行状态</div>
              <div>
                <StatusBadge status={chainQuery.data.status} />
              </div>
            </div>
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">Chain ID</div>
              <div className="font-mono font-semibold text-slate-800">{chainQuery.data.chain_id}</div>
            </div>
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">网络高度</div>
              <div className="font-mono font-semibold text-slate-800">
                {chainQuery.data.network_height.toLocaleString()}
              </div>
            </div>
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">扫描高度</div>
              <div className="font-mono font-semibold text-slate-800">
                {chainQuery.data.scan_height.toLocaleString()}
              </div>
            </div>
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">检查时间</div>
              <div className="font-mono font-medium text-slate-700">{formatDate(chainQuery.data.checked_at)}</div>
            </div>

            {chainQuery.data.token_scanner && (
              <>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">Token 扫描状态</div>
                  <div>
                    <StatusBadge status={chainQuery.data.token_scanner.status} />
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">Token 扫描高度</div>
                  <div className="font-mono font-semibold text-slate-800">
                    {chainQuery.data.token_scanner.scan_height.toLocaleString()}
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">Token 落后区块</div>
                  <div className="font-mono font-semibold text-slate-800">
                    {chainQuery.data.token_scanner.lag.toLocaleString()}
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">Token 检查时间</div>
                  <div className="font-mono font-medium text-slate-700">
                    {formatDate(chainQuery.data.token_scanner.checked_at)}
                  </div>
                </div>
              </>
            )}

            {chainQuery.data.gas_station && (
              <>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">Gas Station 状态</div>
                  <div>
                    <StatusBadge status={chainQuery.data.gas_station.status} />
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">热钱包 ETH</div>
                  <div className="font-mono font-semibold text-slate-800">
                    {formatETH(chainQuery.data.gas_station.balance_eth)} ETH
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">余额告警阈值</div>
                  <div className="font-mono font-semibold text-slate-800">
                    {formatETH(chainQuery.data.gas_station.minimum_eth)} ETH
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">Gas 检查时间</div>
                  <div className="font-mono font-medium text-slate-700">
                    {formatDate(chainQuery.data.gas_station.checked_at)}
                  </div>
                </div>
              </>
            )}

            {chainQuery.data.token_inventory && (
              <>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">热钱包 Token 状态</div>
                  <div>
                    <StatusBadge status={chainQuery.data.token_inventory.status} />
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">
                    热钱包 {chainQuery.data.token_inventory.symbol}
                  </div>
                  <div className="font-mono font-semibold text-slate-800">
                    {chainQuery.data.token_inventory.balance_units}{' '}
                    <span className="text-[11px] font-normal text-slate-500">units</span>
                  </div>
                </div>
                <div className="space-y-1">
                  <div className="text-[11px] text-slate-400">库存检查时间</div>
                  <div className="font-mono font-medium text-slate-700">
                    {formatDate(chainQuery.data.token_inventory.checked_at)}
                  </div>
                </div>
              </>
            )}
          </div>
        )}
      </Panel>

      {/* Recent Transactions */}
      <Panel>
        <SectionHeader
          title="最近资金流水"
          subtitle="点击任意行查看完整链上详情"
          extra={
            <Link
              to="/transactions"
              className="inline-flex items-center gap-1 text-xs font-medium text-blue-600 hover:text-blue-700"
            >
              <span>全部流水</span>
              <ArrowRight className="w-3.5 h-3.5" />
            </Link>
          }
        />
        {transactionsQuery.isLoading ? (
          <TableSkeleton rows={5} cols={5} />
        ) : transactionsQuery.error ? (
          <QueryState loading={false} error={transactionsQuery.error} retry={() => transactionsQuery.refetch()} />
        ) : transactionsQuery.data && transactionsQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/60 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">类型</th>
                  <th className="py-2.5 px-4">金额</th>
                  <th className="py-2.5 px-4">交易哈希</th>
                  <th className="py-2.5 px-4">状态</th>
                  <th className="py-2.5 px-4 text-right">时间</th>
                  <th className="py-2.5 pr-4 w-8" />
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {transactionsQuery.data.items.map((row) => {
                  const isInbound = ['DEPOSIT', 'TOKEN_DEPOSIT'].includes(row.type)
                  return (
                    <tr
                      key={`${row.type}-${row.id}`}
                      onClick={() => setSelectedDetail(transactionToDetail(row))}
                      className="hover:bg-slate-50/80 transition-colors cursor-pointer group"
                    >
                      <td className="py-3 px-4 whitespace-nowrap">
                        <TransactionType type={row.type} />
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap font-mono font-semibold">
                        <span className={isInbound ? 'text-emerald-600' : 'text-slate-900'}>
                          {isInbound ? '+' : '-'}
                          {formatETH(row.amount)} {row.asset}
                        </span>
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap">
                        <HashLink hash={row.tx_hash} url={row.explorer_url} />
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap">
                        <StatusBadge status={row.status} />
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap text-right font-mono text-slate-500">
                        {formatDate(row.created_at)}
                      </td>
                      <td className="py-3 pr-4 text-right text-slate-300 group-hover:text-slate-600">
                        <ChevronRight className="w-4 h-4 inline" />
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState description="暂无最近资金流水" />
        )}
      </Panel>

      {/* Worker Errors */}
      <Panel>
        <SectionHeader
          title="Worker 异常"
          extra={
            <span className="inline-flex items-center gap-1 text-[11px] text-slate-400">
              <AlertTriangle className="w-3.5 h-3.5 text-amber-500" />
              仅展示最新记录
            </span>
          }
        />
        {errorsQuery.isLoading ? (
          <TableSkeleton rows={3} cols={5} />
        ) : errorsQuery.error ? (
          <QueryState loading={false} error={errorsQuery.error} retry={() => errorsQuery.refetch()} />
        ) : errorsQuery.data && errorsQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/60 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">Worker</th>
                  <th className="py-2.5 px-4">阶段</th>
                  <th className="py-2.5 px-4">错误详情</th>
                  <th className="py-2.5 px-4 text-right">重试</th>
                  <th className="py-2.5 px-4 text-right">最后发生</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {errorsQuery.data.items.map((err) => (
                  <tr
                    key={err.id}
                    onClick={() => setSelectedDetail(workerErrorToDetail(err))}
                    className="hover:bg-rose-50/30 transition-colors cursor-pointer"
                  >
                    <td className="py-2.5 px-4 font-mono font-medium text-slate-800 whitespace-nowrap">
                      {err.worker}
                    </td>
                    <td className="py-2.5 px-4 font-mono text-slate-600 whitespace-nowrap">{err.stage}</td>
                    <td className="py-2.5 px-4 text-rose-700 max-w-md truncate" title={err.error_message}>
                      {err.error_message}
                    </td>
                    <td className="py-2.5 px-4 text-right font-mono text-slate-700">{err.retry_count}</td>
                    <td className="py-2.5 px-4 text-right font-mono text-slate-500 whitespace-nowrap">
                      {formatDate(err.last_occurred_at)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState description="当前没有 Worker 异常" />
        )}
      </Panel>

      <TxDetailDrawer detail={selectedDetail} onClose={() => setSelectedDetail(null)} />
    </div>
  )
}
