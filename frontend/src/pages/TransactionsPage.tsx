import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Segmented, Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { ArrowDownToLine, ArrowUpFromLine } from 'lucide-react'
import { api } from '../api'
import { EmptyState, formatDate, formatETH, HashLink, PageHeader, QueryState, StatusTag } from '../components/Common'
import type { Transaction } from '../types'

type Filter = 'ALL' | 'DEPOSIT' | 'WITHDRAWAL'

export default function TransactionsPage() {
  const [filter, setFilter] = useState<Filter>('ALL')
  const [page, setPage] = useState(1)
  const pageSize = 20
  const query = useQuery({ queryKey: ['transactions', page, pageSize], queryFn: () => api.transactions(page, pageSize) })
  const items = (query.data?.items ?? []).filter((item) => filter === 'ALL' || item.type === filter)
  const columns: ColumnsType<Transaction> = [
    { title: '类型', dataIndex: 'type', width: 120, render: (type) => type === 'DEPOSIT' ? <span className="type-cell deposit"><ArrowDownToLine size={15} />充值</span> : <span className="type-cell withdrawal"><ArrowUpFromLine size={15} />提币</span> },
    { title: '用户 ID', dataIndex: 'user_id', width: 100 },
    { title: '金额', dataIndex: 'amount_eth', width: 160, render: (value, row) => <strong>{row.type === 'DEPOSIT' ? '+' : '-'}{formatETH(value)} ETH</strong> },
    { title: '交易哈希', dataIndex: 'tx_hash', render: (_, row) => <HashLink hash={row.tx_hash} url={row.explorer_url} /> },
    { title: '区块', dataIndex: 'block_number', width: 130, render: (value) => value ?? '--' },
    { title: '确认数', dataIndex: 'confirmations', width: 100 },
    { title: '状态', dataIndex: 'status', width: 130, render: (status) => <StatusTag status={status} /> },
    { title: '时间', dataIndex: 'created_at', width: 165, render: formatDate },
  ]

  return (
    <>
      <PageHeader title="资金流水" extra={<Segmented value={filter} onChange={(value) => setFilter(value as Filter)} options={[{ label: '全部', value: 'ALL' }, { label: '充值', value: 'DEPOSIT' }, { label: '提币', value: 'WITHDRAWAL' }]} />} />
      <section className="panel table-panel">
        <QueryState loading={query.isLoading} error={query.error} retry={() => query.refetch()} />
        {query.data && (items.length ? (
          <Table
            rowKey={(row) => `${row.type}-${row.id}`}
            columns={columns}
            dataSource={items}
            scroll={{ x: 980 }}
            pagination={{ current: page, pageSize, total: query.data.has_more ? page * pageSize + 1 : (page - 1) * pageSize + query.data.items.length, showSizeChanger: false, onChange: setPage }}
          />
        ) : <EmptyState description="当前筛选条件下没有流水" />)}
      </section>
    </>
  )
}
