import { useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Select, Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { ArrowDownToLine, ArrowRightLeft, ArrowUpFromLine, Fuel, PackageCheck } from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { EmptyState, formatDate, formatETH, HashLink, PageHeader, QueryState, StatusTag } from '../components/Common'
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
  if (type === 'DEPOSIT' || type === 'TOKEN_DEPOSIT') return <span className="type-cell deposit"><ArrowDownToLine size={15} />充值</span>
  if (type === 'WITHDRAWAL' || type === 'TOKEN_WITHDRAWAL') return <span className="type-cell withdrawal"><ArrowUpFromLine size={15} />提币</span>
  if (type === 'TOKEN_SWEEP') return <span className="type-cell operation"><PackageCheck size={15} />归集</span>
  if (type === 'GAS_TOPUP') return <span className="type-cell operation"><Fuel size={15} />补 Gas</span>
  return <span className="type-cell operation"><ArrowRightLeft size={15} />{type}</span>
}

export default function TransactionsPage() {
  const { assets, asset } = useAppContext()
  const [assetFilter, setAssetFilter] = useState(asset.symbol)
  const [typeFilter, setTypeFilter] = useState('')
  const [page, setPage] = useState(1)
  const pageSize = 20

  useEffect(() => {
    setAssetFilter(asset.symbol)
    setPage(1)
  }, [asset.symbol])

  const query = useQuery({
    queryKey: ['transactions', page, pageSize, assetFilter, typeFilter],
    queryFn: () => api.transactions(page, pageSize, assetFilter, typeFilter),
  })
  const columns: ColumnsType<Transaction> = [
    { title: '类型', dataIndex: 'type', width: 120, render: (type) => <TransactionType type={type} /> },
    { title: '用户 ID', dataIndex: 'user_id', width: 100, render: (value) => value || '--' },
    { title: '资产', dataIndex: 'asset', width: 90 },
    { title: '金额', dataIndex: 'amount', width: 170, render: (value, row) => <strong>{['DEPOSIT', 'TOKEN_DEPOSIT'].includes(row.type) ? '+' : '-'}{formatETH(value)} {row.asset}</strong> },
    { title: '交易哈希', dataIndex: 'tx_hash', render: (_, row) => <HashLink hash={row.tx_hash} url={row.explorer_url} /> },
    { title: '区块', dataIndex: 'block_number', width: 130, render: (value) => value ?? '--' },
    { title: '确认数', dataIndex: 'confirmations', width: 100 },
    { title: '状态', dataIndex: 'status', width: 130, render: (status) => <StatusTag status={status} /> },
    { title: '时间', dataIndex: 'created_at', width: 165, render: formatDate },
  ]

  const resetPage = (setter: (value: string) => void) => (value: string) => {
    setter(value)
    setPage(1)
  }

  return (
    <>
      <PageHeader
        title="资金流水"
        extra={(
          <div className="transaction-filters">
            <Select aria-label="按资产筛选" value={assetFilter} onChange={resetPage(setAssetFilter)} options={[{ value: '', label: '全部资产' }, ...assets.map((item) => ({ value: item.symbol, label: item.symbol }))]} />
            <Select aria-label="按类型筛选" value={typeFilter} onChange={resetPage(setTypeFilter)} options={typeOptions} />
          </div>
        )}
      />
      <section className="panel table-panel">
        <QueryState loading={query.isLoading} error={query.error} retry={() => query.refetch()} />
        {query.data && (query.data.items.length ? (
          <Table
            rowKey={(row) => `${row.type}-${row.id}`}
            columns={columns}
            dataSource={query.data.items}
            scroll={{ x: 1120 }}
            pagination={{ current: page, pageSize, total: query.data.has_more ? page * pageSize + 1 : (page - 1) * pageSize + query.data.items.length, showSizeChanger: false, onChange: setPage }}
          />
        ) : <EmptyState description="当前筛选条件下没有流水" />)}
      </section>
    </>
  )
}
