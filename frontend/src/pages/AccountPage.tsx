import { useQuery } from '@tanstack/react-query'
import { Table, Tag } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { AddressText, formatDate, formatETH, PageHeader, QueryState, SectionHeader } from '../components/Common'
import type { MultiAssetBalance } from '../types'

export default function AccountPage() {
  const { userId, users } = useAppContext()
  const user = users.find((item) => item.id === userId)
  const walletQuery = useQuery({ queryKey: ['wallet', userId], queryFn: () => api.wallet(userId), enabled: userId > 0 })
  const balancesQuery = useQuery({ queryKey: ['balances', userId], queryFn: () => api.balances(userId), enabled: userId > 0 })
  const columns: ColumnsType<MultiAssetBalance> = [
    { title: '资产', dataIndex: 'asset', width: 110, render: (value, row) => <span className="asset-name"><strong>{value}</strong><Tag>{row.asset_type}</Tag></span> },
    { title: '可用余额', dataIndex: 'available', width: 180, render: (value, row) => <strong>{formatETH(value)} {row.asset}</strong> },
    { title: '待确认充值', dataIndex: 'pending_deposit', width: 170, render: (value, row) => `${formatETH(value)} ${row.asset}` },
    { title: '处理中提币', dataIndex: 'pending_withdrawal', width: 170, render: (value, row) => `${formatETH(value)} ${row.asset}` },
    { title: '精度', dataIndex: 'decimals', width: 90 },
    { title: '合约', dataIndex: 'contract_address', render: (value) => value ? <AddressText value={value} /> : <span className="muted">原生资产</span> },
  ]

  return (
    <>
      <PageHeader title="托管账户" />
      <section className="panel">
        <SectionHeader title="账户信息" />
        <div className="detail-grid">
          <div><span>用户名称</span><strong>{user?.display_name ?? '--'}</strong></div>
          <div><span>用户编码</span><strong className="mono">{user?.code ?? '--'}</strong></div>
          <div><span>用户 ID</span><strong>{user?.id ?? '--'}</strong></div>
          <div><span>创建时间</span><strong>{user ? formatDate(user.created_at) : '--'}</strong></div>
        </div>
      </section>
      <section className="panel">
        <SectionHeader title="Sepolia 托管钱包" />
        <QueryState loading={walletQuery.isLoading} error={walletQuery.error} retry={() => walletQuery.refetch()} />
        {walletQuery.data && <div className="wallet-address"><span>统一充值地址</span><AddressText value={walletQuery.data.address} /></div>}
      </section>
      <section className="panel table-panel">
        <SectionHeader title="多资产余额" />
        <QueryState loading={balancesQuery.isLoading} error={balancesQuery.error} retry={() => balancesQuery.refetch()} />
        {balancesQuery.data && <Table rowKey="asset_id" columns={columns} dataSource={balancesQuery.data.items} pagination={false} scroll={{ x: 960 }} />}
      </section>
    </>
  )
}
