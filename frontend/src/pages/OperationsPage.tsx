import { useQuery } from '@tanstack/react-query'
import { Table, Tooltip } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { Activity, Fuel, PackageCheck, Radio, WalletCards } from 'lucide-react'
import { api } from '../api'
import { AddressText, EmptyState, formatDate, formatETH, HashLink, PageHeader, QueryState, SectionHeader, StatusTag } from '../components/Common'
import type { InternalTransfer, TokenSweep, WorkerError } from '../types'

export default function OperationsPage() {
  const platformQuery = useQuery({ queryKey: ['platform-wallet'], queryFn: api.platformWallet })
  const sweepsQuery = useQuery({ queryKey: ['sweeps', 1, 20], queryFn: () => api.sweeps(1, 20) })
  const transfersQuery = useQuery({ queryKey: ['internal-transfers', 1, 20], queryFn: () => api.internalTransfers(1, 20) })
  const errorsQuery = useQuery({ queryKey: ['worker-errors', 1, 10], queryFn: () => api.workerErrors(1, 10) })

  const sweepColumns: ColumnsType<TokenSweep> = [
    { title: '用户 ID', dataIndex: 'user_id', width: 90 },
    { title: '资产', dataIndex: 'asset', width: 90 },
    { title: '识别金额', dataIndex: 'recognized_amount', render: (value, row) => `${value} ${row.asset}` },
    { title: '归集金额', dataIndex: 'sweep_amount', render: (value, row) => value ? `${value} ${row.asset}` : '--' },
    { title: '交易哈希', dataIndex: 'tx_hash', render: (_, row) => <HashLink hash={row.tx_hash} url={row.explorer_url} /> },
    { title: '确认数', dataIndex: 'confirmations', width: 90 },
    { title: '状态', dataIndex: 'status', width: 120, render: (status) => <StatusTag status={status} /> },
    { title: '时间', dataIndex: 'created_at', width: 160, render: formatDate },
    { title: '错误', dataIndex: 'error_message', ellipsis: true, render: (value) => value ? <Tooltip title={value}><span className="error-text">{value}</span></Tooltip> : '--' },
  ]
  const transferColumns: ColumnsType<InternalTransfer> = [
    { title: '类型', dataIndex: 'transfer_type', width: 130 },
    { title: '归集 ID', dataIndex: 'sweep_id', width: 90 },
    { title: '来源', dataIndex: 'from_address', render: (value) => <AddressText value={value} /> },
    { title: '目标', dataIndex: 'to_address', render: (value) => <AddressText value={value} /> },
    { title: '金额', dataIndex: 'amount_eth', width: 130, render: (value) => `${formatETH(value)} ETH` },
    { title: '交易哈希', dataIndex: 'tx_hash', render: (_, row) => <HashLink hash={row.tx_hash} url={row.explorer_url} /> },
    { title: '状态', dataIndex: 'status', width: 120, render: (status) => <StatusTag status={status} /> },
  ]
  const errorColumns: ColumnsType<WorkerError> = [
    { title: 'Worker', dataIndex: 'worker', width: 150 },
    { title: '阶段', dataIndex: 'stage', width: 130 },
    { title: '错误', dataIndex: 'error_message', ellipsis: true, render: (value) => <Tooltip title={value}><span className="error-text">{value}</span></Tooltip> },
    { title: '重试', dataIndex: 'retry_count', width: 80 },
    { title: '最后发生', dataIndex: 'last_occurred_at', width: 160, render: formatDate },
  ]

  return (
    <>
      <PageHeader title="链上运维" />
      <section className="operations-metrics">
        <div className="operation-metric"><WalletCards size={18} /><span>平台热钱包</span><strong>{platformQuery.data?.address ? <AddressText value={platformQuery.data.address} /> : '--'}</strong></div>
        <div className="operation-metric"><Fuel size={18} /><span>ETH Gas 余额</span><strong>{platformQuery.data ? `${formatETH(platformQuery.data.eth_balance)} ETH` : '--'}</strong><StatusTag status={platformQuery.data?.gas_status ?? 'CHECKING'} /></div>
        <div className="operation-metric"><PackageCheck size={18} /><span>Token 库存</span><strong>{platformQuery.data?.token_balance ? `${platformQuery.data.token_balance} ${platformQuery.data.token_symbol ?? ''}` : '--'}</strong><StatusTag status={platformQuery.data?.token_status ?? 'CHECKING'} /></div>
        <div className="operation-metric"><Activity size={18} /><span>下一个 Nonce</span><strong>{platformQuery.data?.next_nonce ?? '--'}</strong></div>
      </section>
      <section className="panel operation-panel">
        <SectionHeader title="平台热钱包状态" extra={<span className="section-note"><Radio size={15} />仅展示运行状态</span>} />
        <QueryState loading={platformQuery.isLoading} error={platformQuery.error} retry={() => platformQuery.refetch()} />
        {platformQuery.data && <div className="operation-detail-grid">
          <div><span>网络</span><strong>{platformQuery.data.network}</strong></div>
          <div><span>角色</span><strong>{platformQuery.data.role}</strong></div>
          <div><span>地址</span><AddressText value={platformQuery.data.address} /></div>
          <div><span>最近错误</span><strong className="error-text">{platformQuery.data.last_error || '无'}</strong></div>
        </div>}
      </section>
      <section className="panel table-panel operation-panel">
        <SectionHeader title="Token 归集任务" />
        <QueryState loading={sweepsQuery.isLoading} error={sweepsQuery.error} retry={() => sweepsQuery.refetch()} />
        {sweepsQuery.data && (sweepsQuery.data.items.length ? <Table rowKey="id" columns={sweepColumns} dataSource={sweepsQuery.data.items} pagination={false} scroll={{ x: 1120 }} /> : <EmptyState description="暂无 Token 归集任务" />)}
      </section>
      <section className="panel table-panel operation-panel">
        <SectionHeader title="Gas 补充转账" />
        <QueryState loading={transfersQuery.isLoading} error={transfersQuery.error} retry={() => transfersQuery.refetch()} />
        {transfersQuery.data && (transfersQuery.data.items.length ? <Table rowKey="id" columns={transferColumns} dataSource={transfersQuery.data.items} pagination={false} scroll={{ x: 1120 }} /> : <EmptyState description="暂无 Gas 补充转账" />)}
      </section>
      <section className="panel table-panel operation-panel">
        <SectionHeader title="Worker 异常" />
        <QueryState loading={errorsQuery.isLoading} error={errorsQuery.error} retry={() => errorsQuery.refetch()} />
        {errorsQuery.data && (errorsQuery.data.items.length ? <Table rowKey="id" columns={errorColumns} dataSource={errorsQuery.data.items} pagination={false} scroll={{ x: 760 }} /> : <EmptyState description="当前没有 Worker 异常" />)}
      </section>
    </>
  )
}
