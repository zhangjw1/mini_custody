import { useQuery } from '@tanstack/react-query'
import { Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { AlertTriangle, ArrowDownToLine, ArrowUpFromLine, Blocks, CircleGauge } from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { EmptyState, formatDate, formatETH, HashLink, PageHeader, QueryState, SectionHeader, StatusTag } from '../components/Common'
import type { Transaction, WorkerError } from '../types'

function TransactionType({ type }: { type: Transaction['type'] }) {
  if (type === 'DEPOSIT' || type === 'TOKEN_DEPOSIT') return <span className="type-cell deposit"><ArrowDownToLine size={15} />充值</span>
  if (type === 'WITHDRAWAL' || type === 'TOKEN_WITHDRAWAL') return <span className="type-cell withdrawal"><ArrowUpFromLine size={15} />提币</span>
  return <span className="type-cell operation"><Blocks size={15} />{type === 'TOKEN_SWEEP' ? '归集' : '补 Gas'}</span>
}

export default function DashboardPage() {
  const { userId, asset } = useAppContext()
  const balancesQuery = useQuery({ queryKey: ['balances', userId], queryFn: () => api.balances(userId), enabled: userId > 0 })
  const chainQuery = useQuery({ queryKey: ['chain'], queryFn: api.chain, refetchInterval: 15_000 })
  const transactionsQuery = useQuery({ queryKey: ['transactions', 1, 5, asset.symbol, ''], queryFn: () => api.transactions(1, 5, asset.symbol) })
  const errorsQuery = useQuery({ queryKey: ['worker-errors', 1, 5], queryFn: () => api.workerErrors(1, 5) })

  const columns: ColumnsType<Transaction> = [
    { title: '类型', dataIndex: 'type', width: 110, render: (type) => <TransactionType type={type} /> },
    { title: '金额', dataIndex: 'amount', width: 150, render: (value, row) => <strong>{['DEPOSIT', 'TOKEN_DEPOSIT'].includes(row.type) ? '+' : '-'}{formatETH(value)} {row.asset}</strong> },
    { title: '交易哈希', dataIndex: 'tx_hash', render: (_, row) => <HashLink hash={row.tx_hash} url={row.explorer_url} /> },
    { title: '状态', dataIndex: 'status', width: 130, render: (status) => <StatusTag status={status} /> },
    { title: '时间', dataIndex: 'created_at', width: 160, render: formatDate },
  ]

  const errorColumns: ColumnsType<WorkerError> = [
    { title: 'Worker', dataIndex: 'worker', width: 150 },
    { title: '阶段', dataIndex: 'stage', width: 140 },
    { title: '错误', dataIndex: 'error_message', ellipsis: true },
    { title: '重试', dataIndex: 'retry_count', width: 80 },
    { title: '最后发生', dataIndex: 'last_occurred_at', width: 160, render: formatDate },
  ]

  return (
    <>
      <PageHeader title="资产总览" />
      <div className="metrics-band">
        <div className="metric"><span className="metric-icon"><CircleGauge size={18} /></span><span className="metric-label">{asset.symbol} 可用余额</span><strong>{balancesQuery.data ? formatETH(balancesQuery.data.items.find((item) => item.asset === asset.symbol)?.available ?? '0') : '--'} <small>{asset.symbol}</small></strong></div>
        <div className="metric"><span className="metric-icon"><ArrowDownToLine size={18} /></span><span className="metric-label">待确认充值</span><strong>{balancesQuery.data ? formatETH(balancesQuery.data.items.find((item) => item.asset === asset.symbol)?.pending_deposit ?? '0') : '--'} <small>{asset.symbol}</small></strong></div>
        <div className="metric"><span className="metric-icon"><ArrowUpFromLine size={18} /></span><span className="metric-label">处理中提币</span><strong>{balancesQuery.data ? formatETH(balancesQuery.data.items.find((item) => item.asset === asset.symbol)?.pending_withdrawal ?? '0') : '--'} <small>{asset.symbol}</small></strong></div>
        <div className="metric"><span className="metric-icon"><Blocks size={18} /></span><span className="metric-label">扫描落后区块</span><strong>{chainQuery.data?.lag ?? '--'} <small>blocks</small></strong></div>
      </div>

      <section className="panel chain-panel">
        <SectionHeader title="链上服务" />
        <QueryState loading={chainQuery.isLoading} error={chainQuery.error} retry={() => chainQuery.refetch()} />
        {chainQuery.data && (
          <div className="chain-grid">
            <div><span>运行状态</span><StatusTag status={chainQuery.data.status} /></div>
            <div><span>Chain ID</span><strong>{chainQuery.data.chain_id}</strong></div>
            <div><span>网络高度</span><strong>{chainQuery.data.network_height.toLocaleString()}</strong></div>
            <div><span>扫描高度</span><strong>{chainQuery.data.scan_height.toLocaleString()}</strong></div>
            <div><span>检查时间</span><strong>{formatDate(chainQuery.data.checked_at)}</strong></div>
            {chainQuery.data.token_scanner && <div><span>Token 扫描状态</span><StatusTag status={chainQuery.data.token_scanner.status} /></div>}
            {chainQuery.data.token_scanner && <div><span>Token 扫描高度</span><strong>{chainQuery.data.token_scanner.scan_height.toLocaleString()}</strong></div>}
            {chainQuery.data.token_scanner && <div><span>Token 落后区块</span><strong>{chainQuery.data.token_scanner.lag.toLocaleString()}</strong></div>}
            {chainQuery.data.token_scanner && <div><span>Token 检查时间</span><strong>{formatDate(chainQuery.data.token_scanner.checked_at)}</strong></div>}
			{chainQuery.data.gas_station && <div><span>Gas Station 状态</span><StatusTag status={chainQuery.data.gas_station.status} /></div>}
			{chainQuery.data.gas_station && <div><span>热钱包 ETH</span><strong>{formatETH(chainQuery.data.gas_station.balance_eth)} ETH</strong></div>}
			{chainQuery.data.gas_station && <div><span>余额告警阈值</span><strong>{formatETH(chainQuery.data.gas_station.minimum_eth)} ETH</strong></div>}
			{chainQuery.data.gas_station && <div><span>Gas 检查时间</span><strong>{formatDate(chainQuery.data.gas_station.checked_at)}</strong></div>}
			{chainQuery.data.token_inventory && <div><span>热钱包 Token 状态</span><StatusTag status={chainQuery.data.token_inventory.status} /></div>}
			{chainQuery.data.token_inventory && <div><span>热钱包 {chainQuery.data.token_inventory.symbol}</span><strong>{chainQuery.data.token_inventory.balance_units} <small>units</small></strong></div>}
			{chainQuery.data.token_inventory && <div><span>库存检查时间</span><strong>{formatDate(chainQuery.data.token_inventory.checked_at)}</strong></div>}
          </div>
        )}
      </section>

      <section className="panel">
        <SectionHeader title="最近资金流水" />
        <QueryState loading={transactionsQuery.isLoading} error={transactionsQuery.error} retry={() => transactionsQuery.refetch()} />
        {transactionsQuery.data && (transactionsQuery.data.items.length ? <Table rowKey={(row) => `${row.type}-${row.id}`} columns={columns} dataSource={transactionsQuery.data.items} pagination={false} scroll={{ x: 760 }} /> : <EmptyState />)}
      </section>

      <section className="panel">
        <SectionHeader title="Worker 异常" extra={<span className="section-note"><AlertTriangle size={15} />仅展示最新记录</span>} />
        <QueryState loading={errorsQuery.isLoading} error={errorsQuery.error} retry={() => errorsQuery.refetch()} />
        {errorsQuery.data && (errorsQuery.data.items.length ? <Table rowKey="id" columns={errorColumns} dataSource={errorsQuery.data.items} pagination={false} scroll={{ x: 760 }} /> : <EmptyState description="当前没有 Worker 异常" />)}
      </section>
    </>
  )
}
