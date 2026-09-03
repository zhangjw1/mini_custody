import { useQuery } from '@tanstack/react-query'
import { Alert, Table } from 'antd'
import type { ColumnsType } from 'antd/es/table'
import { QRCodeSVG } from 'qrcode.react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { AddressText, EmptyState, formatDate, formatETH, HashLink, PageHeader, QueryState, SectionHeader, StatusTag } from '../components/Common'
import type { BitcoinDeposit, BitcoinWallet, Deposit, Page, TokenDeposit, Wallet } from '../types'

export default function DepositPage() {
  const { userId, asset } = useAppContext()
  const isToken = asset.asset_type === 'ERC20'
  const isBitcoin = asset.network.startsWith('bitcoin-')
  const walletQuery = useQuery<Wallet | BitcoinWallet>({ queryKey: ['wallet', asset.network, userId], queryFn: async () => isBitcoin ? api.bitcoinWallet(userId) : api.wallet(userId), enabled: userId > 0 })
  const depositsQuery = useQuery({
    queryKey: ['deposits', asset.symbol, userId, 1, 20],
    queryFn: async (): Promise<Page<Deposit | TokenDeposit> | Page<BitcoinDeposit>> => {
      if (isBitcoin) return api.bitcoinDeposits(userId)
      if (isToken) return api.tokenDeposits(userId)
      return api.deposits(userId)
    },
    enabled: userId > 0,
  })
  const columns: ColumnsType<Deposit | TokenDeposit> = [
    { title: '金额', width: 150, render: (_, row) => <strong>+{formatETH('amount' in row ? row.amount : row.amount_eth)} {asset.symbol}</strong> },
    { title: '交易哈希', dataIndex: 'tx_hash', render: (_, row) => <HashLink hash={row.tx_hash} url={row.explorer_url} /> },
    { title: '区块', dataIndex: 'block_number', width: 130, render: (value, row) => <a href={row.block_url} target="_blank" rel="noreferrer">{value}</a> },
    { title: '确认数', dataIndex: 'confirmations', width: 100 },
    { title: '状态', dataIndex: 'status', width: 120, render: (status) => <StatusTag status={status} /> },
    { title: '时间', dataIndex: 'created_at', width: 160, render: formatDate },
  ]

  return (
    <>
      <PageHeader title={`充值 ${asset.symbol}`} />
      <div className="deposit-layout">
        <section className="panel deposit-address-panel">
          <SectionHeader title={isBitcoin ? `Bitcoin ${asset.network === 'bitcoin-testnet4' ? 'Testnet4' : 'Signet'} 充值地址` : 'Sepolia 充值地址'} />
          <QueryState loading={walletQuery.isLoading} error={walletQuery.error} retry={() => walletQuery.refetch()} />
          {walletQuery.data && (
            <div className="qr-layout">
              <div className="qr-box"><QRCodeSVG value={walletQuery.data.address} size={168} level="M" includeMargin /></div>
              <div className="deposit-address-content">
                <span className="field-label">钱包地址</span>
                <AddressText value={walletQuery.data.address} />
                <div className="network-row"><span>网络</span><strong>{isBitcoin ? `Bitcoin ${asset.network === 'bitcoin-testnet4' ? 'Testnet4' : 'Signet'}` : 'Ethereum Sepolia'}</strong></div>
                {isBitcoin && <div className="network-row"><span>地址类型</span><strong>P2WPKH</strong></div>}
                {isToken && <div className="network-row"><span>Token 合约</span><AddressText value={asset.contract_address ?? ''} /></div>}
                {isToken && <div className="network-row"><span>Decimals</span><strong>{asset.decimals}</strong></div>}
                <Alert type="warning" showIcon title={`仅向该地址发送 ${isBitcoin ? `Bitcoin ${asset.network === 'bitcoin-testnet4' ? 'Testnet4' : 'Signet'} BTC` : `Sepolia ${asset.symbol}`}，使用其他网络可能无法找回。`} />
              </div>
            </div>
          )}
        </section>
      </div>
      <section className="panel table-panel">
        <SectionHeader title={`${asset.symbol} 充值记录`} />
        <QueryState loading={depositsQuery.isLoading} error={depositsQuery.error} retry={() => depositsQuery.refetch()} />
        {isBitcoin ? depositsQuery.data && ('items' in depositsQuery.data) && (depositsQuery.data.items.length ? <Table rowKey="id" columns={[{title:'金额',render:(_,row)=><strong>+{row.amount_sats} sat BTC</strong>},{title:'交易 ID',dataIndex:'txid',render:(value)=><HashLink hash={value} />},{title:'输出',dataIndex:'vout'},{title:'区块',dataIndex:'block_height'},{title:'确认数',dataIndex:'confirmations'},{title:'状态',dataIndex:'status',render:(status)=><StatusTag status={status} />}]} dataSource={depositsQuery.data.items as BitcoinDeposit[]} pagination={false} scroll={{x:820}} /> : <EmptyState description="尚未检测到 BTC 充值" />) : depositsQuery.data && (depositsQuery.data.items.length ? <Table rowKey="id" columns={columns} dataSource={depositsQuery.data.items as (Deposit|TokenDeposit)[]} pagination={false} scroll={{ x: 820 }} /> : <EmptyState description={`尚未检测到 ${asset.symbol} 充值`} />)}
      </section>
    </>
  )
}
