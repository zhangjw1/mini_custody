import { useQuery } from '@tanstack/react-query'
import { Alert, Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { QRCodeSVG } from 'qrcode.react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { AddressText, EmptyState, formatDate, formatETH, HashLink, PageHeader, QueryState, SectionHeader, StatusTag } from '../components/Common'
import type { Deposit } from '../types'

export default function DepositPage() {
  const { userId } = useAppContext()
  const walletQuery = useQuery({ queryKey: ['wallet', userId], queryFn: () => api.wallet(userId), enabled: userId > 0 })
  const depositsQuery = useQuery({ queryKey: ['deposits', userId, 1, 20], queryFn: () => api.deposits(userId), enabled: userId > 0 })
  const columns: ColumnsType<Deposit> = [
    { title: '金额', dataIndex: 'amount_eth', width: 150, render: (value) => <strong>+{formatETH(value)} ETH</strong> },
    { title: '交易哈希', dataIndex: 'tx_hash', render: (_, row) => <HashLink hash={row.tx_hash} url={row.explorer_url} /> },
    { title: '区块', dataIndex: 'block_number', width: 130, render: (value, row) => <a href={row.block_url} target="_blank" rel="noreferrer">{value}</a> },
    { title: '确认数', dataIndex: 'confirmations', width: 100 },
    { title: '状态', dataIndex: 'status', width: 120, render: (status) => <StatusTag status={status} /> },
    { title: '时间', dataIndex: 'created_at', width: 160, render: formatDate },
  ]

  return (
    <>
      <PageHeader title="充值 ETH" />
      <div className="deposit-layout">
        <section className="panel deposit-address-panel">
          <SectionHeader title="Sepolia 充值地址" />
          <QueryState loading={walletQuery.isLoading} error={walletQuery.error} retry={() => walletQuery.refetch()} />
          {walletQuery.data && (
            <div className="qr-layout">
              <div className="qr-box"><QRCodeSVG value={walletQuery.data.address} size={168} level="M" includeMargin /></div>
              <div className="deposit-address-content">
                <span className="field-label">钱包地址</span>
                <AddressText value={walletQuery.data.address} />
                <div className="network-row"><span>网络</span><strong>Ethereum Sepolia</strong></div>
                <Alert type="warning" showIcon title="仅向该地址发送 Sepolia ETH，发送其他资产可能无法找回。" />
              </div>
            </div>
          )}
        </section>
      </div>
      <section className="panel">
        <SectionHeader title="充值记录" />
        <QueryState loading={depositsQuery.isLoading} error={depositsQuery.error} retry={() => depositsQuery.refetch()} />
        {depositsQuery.data && (depositsQuery.data.items.length ? <Table rowKey="id" columns={columns} dataSource={depositsQuery.data.items} pagination={false} scroll={{ x: 820 }} /> : <EmptyState description="尚未检测到充值" />)}
      </section>
    </>
  )
}
