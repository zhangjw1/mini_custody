import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Activity, Fuel, PackageCheck, Radio, WalletCards } from 'lucide-react'
import { api } from '../api'
import {
  AddressText,
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
import { TxDetailDrawer, type TxDetailData } from '../components/TxDetailDrawer'
import { TableSkeleton } from '../components/ui/Skeleton'
import type { BitcoinSweep, InternalTransfer, TokenSweep, WorkerError } from '../types'

function btcSweepToDetail(row: BitcoinSweep): TxDetailData {
  return {
    title: 'BTC 归集任务详情',
    subtitle: `BTC Sweep #${row.id}`,
    status: row.status,
    amountDisplay: `${row.output_sats.toLocaleString()} sat`,
    txHash: row.txid,
    fields: [
      { label: '任务 ID', value: `#${row.id}`, mono: true },
      { label: '来源地址', value: row.source_address, mono: true, copyable: true },
      { label: '目标地址', value: row.destination_address, mono: true, copyable: true },
      { label: '输入金额', value: `${row.input_sats.toLocaleString()} sat`, mono: true },
      { label: '输出金额', value: `${row.output_sats.toLocaleString()} sat`, mono: true },
      { label: '矿工费', value: `${row.fee_sats.toLocaleString()} sat`, mono: true },
      { label: '费率', value: `${row.fee_rate_sat_vb} sat/vB`, mono: true },
    ],
  }
}

function tokenSweepToDetail(row: TokenSweep): TxDetailData {
  return {
    title: 'Token 归集任务详情',
    subtitle: `Token Sweep #${row.id}`,
    status: row.status,
    amountDisplay: row.sweep_amount
      ? `${row.sweep_amount} ${row.asset}`
      : `${row.recognized_amount} ${row.asset}`,
    txHash: row.tx_hash,
    explorerUrl: row.explorer_url,
    confirmations: row.confirmations,
    errorMessage: row.error_message,
    fields: [
      { label: '归集 ID', value: `#${row.id}`, mono: true },
      { label: '用户 ID', value: String(row.user_id), mono: true },
      { label: '资产', value: row.asset, mono: true },
      { label: '识别金额', value: `${row.recognized_amount} ${row.asset}`, mono: true },
      {
        label: '归集金额',
        value: row.sweep_amount ? `${row.sweep_amount} ${row.asset}` : '--',
        mono: true,
      },
      {
        label: 'Gas 补充 ID',
        value: row.gas_topup_transfer_id !== undefined ? `#${row.gas_topup_transfer_id}` : '--',
        mono: true,
      },
      {
        label: '实际手续费',
        value: row.actual_fee_eth ? `${row.actual_fee_eth} ETH` : '--',
        mono: true,
      },
      { label: '确认数', value: String(row.confirmations), mono: true },
      { label: '创建时间', value: formatDate(row.created_at), mono: true },
      { label: '更新时间', value: formatDate(row.updated_at), mono: true },
      ...(row.error_message
        ? [{ label: '错误信息', value: row.error_message, mono: true, copyable: true }]
        : []),
    ],
  }
}

function transferToDetail(row: InternalTransfer): TxDetailData {
  return {
    title: 'Gas 补充内部转账详情',
    subtitle: `${row.transfer_type} #${row.id}`,
    status: row.status,
    amountDisplay: `${formatETH(row.amount_eth)} ETH`,
    txHash: row.tx_hash,
    explorerUrl: row.explorer_url,
    confirmations: row.confirmations,
    errorMessage: row.error_message,
    fields: [
      { label: '转账 ID', value: `#${row.id}`, mono: true },
      { label: '归集 ID', value: `#${row.sweep_id}`, mono: true },
      { label: '转账类型', value: row.transfer_type, mono: true },
      { label: '来源地址', value: row.from_address, mono: true, copyable: true },
      { label: '目标地址', value: row.to_address, mono: true, copyable: true },
      { label: '金额 (ETH)', value: `${row.amount_eth} ETH`, mono: true },
      {
        label: '实际手续费',
        value: row.actual_fee_eth ? `${row.actual_fee_eth} ETH` : '--',
        mono: true,
      },
      { label: '确认数', value: String(row.confirmations), mono: true },
      { label: '创建时间', value: formatDate(row.created_at), mono: true },
    ],
  }
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
      {
        label: '关联 ID',
        value: err.reference_id !== undefined ? String(err.reference_id) : '--',
        mono: true,
      },
      { label: '重试次数', value: String(err.retry_count), mono: true },
      { label: '首次发生', value: formatDate(err.first_occurred_at), mono: true },
      { label: '最后发生', value: formatDate(err.last_occurred_at), mono: true },
      { label: '完整错误信息', value: err.error_message, mono: true, copyable: true },
    ],
  }
}

export default function OperationsPage() {
  const [selectedDetail, setSelectedDetail] = useState<TxDetailData | null>(null)

  const chainQuery = useQuery({
    queryKey: ['chain'],
    queryFn: api.chain,
    refetchInterval: 15_000,
  })

  const capabilities = chainQuery.data?.capabilities
  const bitcoinEnabled = Boolean(capabilities?.bitcoin_enabled)
  const erc20Enabled = Boolean(capabilities?.erc20_enabled)

  const platformQuery = useQuery({
    queryKey: ['platform-wallet'],
    queryFn: api.platformWallet,
    refetchInterval: 10_000,
  })
  const sweepsQuery = useQuery({
    queryKey: ['sweeps', 1, 20],
    queryFn: () => api.sweeps(1, 20),
    enabled: erc20Enabled,
    refetchInterval: 10_000,
  })
  const transfersQuery = useQuery({
    queryKey: ['internal-transfers', 1, 20],
    queryFn: () => api.internalTransfers(1, 20),
    enabled: erc20Enabled,
    refetchInterval: 10_000,
  })
  const errorsQuery = useQuery({
    queryKey: ['worker-errors', 1, 10],
    queryFn: () => api.workerErrors(1, 10),
    refetchInterval: 15_000,
  })
  const bitcoinSweepsQuery = useQuery({
    queryKey: ['bitcoin-sweeps', 1, 20],
    queryFn: () => api.bitcoinSweeps(1, 20),
    enabled: bitcoinEnabled,
    refetchInterval: 10_000,
  })

  return (
    <div className="space-y-5">
      <PageHeader
        title="链上运维"
        subtitle="平台热钱包水位、资产归集调度与 Worker 运行监控（每 10 秒自动刷新）"
        extra={
          capabilities && (
            <div className="flex items-center gap-2 text-xs">
              <StatusBadge status={erc20Enabled ? 'ERC20_ENABLED' : 'ERC20_DISABLED'} />
              <StatusBadge status={bitcoinEnabled ? 'BTC_ENABLED' : 'BTC_DISABLED'} />
            </div>
          )
        }
      />

      {/* Operations Metrics Cards */}
      <section className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-3">
        <div className="bg-white border border-slate-200 rounded-lg p-4 space-y-2 shadow-2xs">
          <div className="flex items-center justify-between text-xs text-slate-500">
            <span>平台热钱包</span>
            <WalletCards className="w-4 h-4 text-[#09182b]" />
          </div>
          <div className="min-w-0">
            {platformQuery.data?.address ? (
              <AddressText value={platformQuery.data.address} short />
            ) : (
              <strong className="font-mono text-sm text-slate-400">--</strong>
            )}
          </div>
        </div>

        <div className="bg-white border border-slate-200 rounded-lg p-4 space-y-2 shadow-2xs">
          <div className="flex items-center justify-between text-xs text-slate-500">
            <span>ETH Gas 余额</span>
            <Fuel className="w-4 h-4 text-[#09182b]" />
          </div>
          <div className="flex items-center justify-between gap-2">
            <strong className="font-mono text-base font-bold text-slate-900 truncate">
              {platformQuery.data ? `${formatETH(platformQuery.data.eth_balance)} ETH` : '--'}
            </strong>
            <StatusBadge status={platformQuery.data?.gas_status ?? 'CHECKING'} />
          </div>
        </div>

        <div className="bg-white border border-slate-200 rounded-lg p-4 space-y-2 shadow-2xs">
          <div className="flex items-center justify-between text-xs text-slate-500">
            <span>Token 库存</span>
            <PackageCheck className="w-4 h-4 text-[#09182b]" />
          </div>
          <div className="flex items-center justify-between gap-2">
            <strong className="font-mono text-base font-bold text-slate-900 truncate">
              {platformQuery.data?.token_balance
                ? `${platformQuery.data.token_balance} ${platformQuery.data.token_symbol ?? ''}`
                : '--'}
            </strong>
            <StatusBadge status={platformQuery.data?.token_status ?? 'CHECKING'} />
          </div>
        </div>

        <div className="bg-white border border-slate-200 rounded-lg p-4 space-y-2 shadow-2xs">
          <div className="flex items-center justify-between text-xs text-slate-500">
            <span>下一个 Nonce</span>
            <Activity className="w-4 h-4 text-[#09182b]" />
          </div>
          <div>
            <strong className="font-mono text-lg font-bold text-slate-900">
              {platformQuery.data?.next_nonce ?? '--'}
            </strong>
          </div>
        </div>
      </section>

      {/* Platform Wallet Detail */}
      <Panel>
        <SectionHeader
          title="平台热钱包状态"
          extra={
            <span className="inline-flex items-center gap-1.5 text-[11px] text-slate-400">
              <Radio className="w-3.5 h-3.5 text-emerald-500" />
              仅展示运行状态
            </span>
          }
        />
        <QueryState
          loading={platformQuery.isLoading}
          error={platformQuery.error}
          retry={() => platformQuery.refetch()}
        />
        {platformQuery.data && (
          <div className="p-4 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 text-xs">
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">网络</div>
              <div className="font-mono font-semibold text-slate-800">{platformQuery.data.network}</div>
            </div>
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">角色</div>
              <div className="font-mono font-semibold text-slate-800">{platformQuery.data.role}</div>
            </div>
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">钱包地址</div>
              <div>
                <AddressText value={platformQuery.data.address} short />
              </div>
            </div>
            <div className="space-y-1">
              <div className="text-[11px] text-slate-400">最近错误</div>
              <div
                className={`font-mono truncate ${
                  platformQuery.data.last_error ? 'text-rose-600 font-medium' : 'text-slate-600'
                }`}
                title={platformQuery.data.last_error || '无'}
              >
                {platformQuery.data.last_error || '无'}
              </div>
            </div>
          </div>
        )}
      </Panel>

      {/* BTC Sweeps (Controlled by bitcoin_enabled) */}
      <Panel>
        <SectionHeader
          title="BTC 归集任务"
          subtitle={
            bitcoinEnabled
              ? `网络 ${capabilities?.bitcoin_network || 'signet'} · 点击任务行查看详情`
              : '未启用 Bitcoin 能力开关'
          }
        />
        {!bitcoinEnabled ? (
          <EmptyState description="当前环境未开启 Bitcoin 模块（capabilities.bitcoin_enabled = false），已跳过请求" />
        ) : bitcoinSweepsQuery.isLoading ? (
          <TableSkeleton rows={4} cols={8} />
        ) : bitcoinSweepsQuery.error ? (
          <QueryState
            loading={false}
            error={bitcoinSweepsQuery.error}
            retry={() => bitcoinSweepsQuery.refetch()}
          />
        ) : bitcoinSweepsQuery.data && bitcoinSweepsQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">来源地址</th>
                  <th className="py-2.5 px-4">目标地址</th>
                  <th className="py-2.5 px-4 text-right">输入 sat</th>
                  <th className="py-2.5 px-4 text-right">输出 sat</th>
                  <th className="py-2.5 px-4 text-right">费用 sat</th>
                  <th className="py-2.5 px-4 text-right">费率 sat/vB</th>
                  <th className="py-2.5 px-4">交易 ID</th>
                  <th className="py-2.5 px-4">状态</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {bitcoinSweepsQuery.data.items.map((row) => (
                  <tr
                    key={row.id}
                    onClick={() => setSelectedDetail(btcSweepToDetail(row))}
                    className="hover:bg-slate-50/80 transition-colors cursor-pointer"
                  >
                    <td className="py-3 px-4 whitespace-nowrap">
                      <AddressText value={row.source_address} short />
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <AddressText value={row.destination_address} short />
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-700">
                      {row.input_sats.toLocaleString()}
                    </td>
                    <td className="py-3 px-4 text-right font-mono font-semibold text-slate-900">
                      {row.output_sats.toLocaleString()}
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-600">
                      {row.fee_sats.toLocaleString()}
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-600">
                      {row.fee_rate_sat_vb}
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <HashLink hash={row.txid} />
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <StatusBadge status={row.status} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState description="暂无 BTC 归集任务" />
        )}
      </Panel>

      {/* Token Sweeps (Controlled by erc20_enabled) */}
      <Panel>
        <SectionHeader
          title="Token 归集任务"
          subtitle={erc20Enabled ? '点击任务行查看详情' : '未启用 ERC-20 能力开关'}
        />
        {!erc20Enabled ? (
          <EmptyState description="当前环境未开启 ERC-20 模块（capabilities.erc20_enabled = false），已跳过请求" />
        ) : sweepsQuery.isLoading ? (
          <TableSkeleton rows={4} cols={9} />
        ) : sweepsQuery.error ? (
          <QueryState loading={false} error={sweepsQuery.error} retry={() => sweepsQuery.refetch()} />
        ) : sweepsQuery.data && sweepsQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">用户 ID</th>
                  <th className="py-2.5 px-4">资产</th>
                  <th className="py-2.5 px-4 text-right">识别金额</th>
                  <th className="py-2.5 px-4 text-right">归集金额</th>
                  <th className="py-2.5 px-4">交易哈希</th>
                  <th className="py-2.5 px-4 text-right">确认数</th>
                  <th className="py-2.5 px-4">状态</th>
                  <th className="py-2.5 px-4 text-right">时间</th>
                  <th className="py-2.5 px-4">错误</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {sweepsQuery.data.items.map((row) => (
                  <tr
                    key={row.id}
                    onClick={() => setSelectedDetail(tokenSweepToDetail(row))}
                    className="hover:bg-slate-50/80 transition-colors cursor-pointer"
                  >
                    <td className="py-3 px-4 font-mono text-slate-700">{row.user_id}</td>
                    <td className="py-3 px-4 font-mono font-semibold text-slate-900">{row.asset}</td>
                    <td className="py-3 px-4 text-right font-mono text-slate-700 whitespace-nowrap">
                      {row.recognized_amount} {row.asset}
                    </td>
                    <td className="py-3 px-4 text-right font-mono font-semibold text-slate-900 whitespace-nowrap">
                      {row.sweep_amount ? `${row.sweep_amount} ${row.asset}` : '--'}
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <HashLink hash={row.tx_hash} url={row.explorer_url} />
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-600">{row.confirmations}</td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <StatusBadge status={row.status} />
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-500 whitespace-nowrap">
                      {formatDate(row.created_at)}
                    </td>
                    <td
                      className="py-3 px-4 max-w-[200px] truncate text-rose-600"
                      title={row.error_message || undefined}
                    >
                      {row.error_message || '--'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState description="暂无 Token 归集任务" />
        )}
      </Panel>

      {/* Gas Topups (Controlled by erc20_enabled) */}
      <Panel>
        <SectionHeader
          title="Gas 补充转账"
          subtitle={erc20Enabled ? '点击记录查看详情' : '未启用 ERC-20 能力开关'}
        />
        {!erc20Enabled ? (
          <EmptyState description="当前环境未开启 ERC-20 模块（capabilities.erc20_enabled = false），已跳过请求" />
        ) : transfersQuery.isLoading ? (
          <TableSkeleton rows={4} cols={7} />
        ) : transfersQuery.error ? (
          <QueryState
            loading={false}
            error={transfersQuery.error}
            retry={() => transfersQuery.refetch()}
          />
        ) : transfersQuery.data && transfersQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">类型</th>
                  <th className="py-2.5 px-4">归集 ID</th>
                  <th className="py-2.5 px-4">来源地址</th>
                  <th className="py-2.5 px-4">目标地址</th>
                  <th className="py-2.5 px-4 text-right">金额</th>
                  <th className="py-2.5 px-4">交易哈希</th>
                  <th className="py-2.5 px-4">状态</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {transfersQuery.data.items.map((row) => (
                  <tr
                    key={row.id}
                    onClick={() => setSelectedDetail(transferToDetail(row))}
                    className="hover:bg-slate-50/80 transition-colors cursor-pointer"
                  >
                    <td className="py-3 px-4 font-mono font-medium text-slate-700 whitespace-nowrap">
                      {row.transfer_type}
                    </td>
                    <td className="py-3 px-4 font-mono text-slate-600">#{row.sweep_id}</td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <AddressText value={row.from_address} short />
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <AddressText value={row.to_address} short />
                    </td>
                    <td className="py-3 px-4 text-right font-mono font-semibold text-slate-900 whitespace-nowrap">
                      {formatETH(row.amount_eth)} ETH
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <HashLink hash={row.tx_hash} url={row.explorer_url} />
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      <StatusBadge status={row.status} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState description="暂无 Gas 补充转账" />
        )}
      </Panel>

      {/* Worker Errors */}
      <Panel>
        <SectionHeader title="Worker 异常" subtitle="点击异常行查看完整报错信息" />
        {errorsQuery.isLoading ? (
          <TableSkeleton rows={3} cols={5} />
        ) : errorsQuery.error ? (
          <QueryState loading={false} error={errorsQuery.error} retry={() => errorsQuery.refetch()} />
        ) : errorsQuery.data && errorsQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">Worker</th>
                  <th className="py-2.5 px-4">阶段</th>
                  <th className="py-2.5 px-4">错误</th>
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
