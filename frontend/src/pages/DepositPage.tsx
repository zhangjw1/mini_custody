import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { QRCodeSVG } from 'qrcode.react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import {
  AddressText,
  AlertBanner,
  CopyButton,
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
import type { BitcoinDeposit, BitcoinWallet, Deposit, Page, TokenDeposit, Wallet } from '../types'

function evmDepositToDetail(row: Deposit | TokenDeposit, symbol: string): TxDetailData {
  const isToken = 'log_index' in row
  const amount = isToken ? row.amount : row.amount_eth
  const fields: TxDetailData['fields'] = [
    { label: '充值 ID', value: `#${row.id}`, mono: true },
    { label: '用户 ID', value: String(row.user_id), mono: true },
    { label: '资产', value: symbol, mono: true },
    { label: '充值金额', value: `+${amount} ${symbol}`, mono: true },
    { label: '区块高度', value: `#${row.block_number.toLocaleString()}`, mono: true, href: row.block_url },
    { label: '确认数', value: String(row.confirmations), mono: true },
    { label: '创建时间', value: formatDate(row.created_at), mono: true },
    { label: '更新时间', value: formatDate(row.updated_at), mono: true },
  ]
  if (isToken) {
    fields.splice(
      4,
      0,
      { label: 'Log Index', value: String(row.log_index), mono: true },
      { label: '发送地址 (From)', value: row.from_address, mono: true, copyable: true },
      { label: '接收地址 (To)', value: row.to_address, mono: true, copyable: true },
      { label: '原始最小单位', value: row.amount_units, mono: true },
    )
  } else {
    fields.splice(4, 0, { label: '金额 (Wei)', value: row.amount_wei, mono: true })
  }

  return {
    title: `${symbol} 充值详情`,
    subtitle: `Deposit #${row.id}`,
    status: row.status,
    amountDisplay: `+${formatETH(amount)} ${symbol}`,
    isInbound: true,
    txHash: row.tx_hash,
    explorerUrl: row.explorer_url,
    blockNumber: row.block_number,
    confirmations: row.confirmations,
    fields,
  }
}

function btcDepositToDetail(row: BitcoinDeposit): TxDetailData {
  return {
    title: 'BTC 充值详情',
    subtitle: `BTC Deposit #${row.id}`,
    status: row.status,
    amountDisplay: `+${row.amount_sats.toLocaleString()} sat BTC`,
    isInbound: true,
    txHash: row.txid,
    blockNumber: row.block_height,
    confirmations: row.confirmations,
    fields: [
      { label: '充值 ID', value: `#${row.id}`, mono: true },
      { label: '输出索引 (vout)', value: String(row.vout), mono: true },
      { label: '区块高度', value: `#${row.block_height.toLocaleString()}`, mono: true },
      { label: '金额 (sat)', value: `${row.amount_sats.toLocaleString()} sat`, mono: true },
      { label: '确认数', value: String(row.confirmations), mono: true },
    ],
  }
}

export default function DepositPage() {
  const { userId, asset } = useAppContext()
  const [selectedDetail, setSelectedDetail] = useState<TxDetailData | null>(null)

  const isToken = asset.asset_type === 'ERC20'
  const isBitcoin = asset.network.startsWith('bitcoin-')
  const btcNetworkLabel = asset.network === 'bitcoin-testnet4' ? 'Testnet4' : 'Signet'

  const walletQuery = useQuery<Wallet | BitcoinWallet>({
    queryKey: ['wallet', asset.network, userId],
    queryFn: async () => (isBitcoin ? api.bitcoinWallet(userId) : api.wallet(userId)),
    enabled: userId > 0,
  })

  const depositsQuery = useQuery({
    queryKey: ['deposits', asset.symbol, userId, 1, 20],
    queryFn: async (): Promise<Page<Deposit | TokenDeposit> | Page<BitcoinDeposit>> => {
      if (isBitcoin) return api.bitcoinDeposits(userId)
      if (isToken) return api.tokenDeposits(userId)
      return api.deposits(userId)
    },
    enabled: userId > 0,
    refetchInterval: 10_000,
  })

  return (
    <div className="space-y-5">
      <PageHeader
        title={`充值 ${asset.symbol}`}
        subtitle="向下方专属托管地址转入资产，每 10 秒自动扫描入账状态"
      />

      <div className="max-w-4xl">
        <Panel>
          <SectionHeader
            title={isBitcoin ? `Bitcoin ${btcNetworkLabel} 充值地址` : 'Sepolia 充值地址'}
            subtitle={isBitcoin ? 'P2WPKH 原生隔离见证地址' : 'EVM 统一入账地址'}
          />
          <QueryState loading={walletQuery.isLoading} error={walletQuery.error} retry={() => walletQuery.refetch()} />
          {walletQuery.data && (
            <div className="p-5 grid grid-cols-1 md:grid-cols-[200px_minmax(0,1fr)] gap-6 items-center">
              <div className="w-[200px] h-[200px] mx-auto md:mx-0 flex items-center justify-center rounded-lg border border-slate-200 bg-white p-2 shadow-2xs">
                <QRCodeSVG value={walletQuery.data.address} size={172} level="M" includeMargin />
              </div>

              <div className="min-w-0 space-y-3 text-xs">
                <div className="space-y-1.5">
                  <div className="text-[11px] font-medium text-slate-500">专属充值钱包地址</div>
                  <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 p-3 rounded-lg border border-slate-200 bg-slate-50/70">
                    <span className="font-mono text-xs sm:text-sm font-semibold text-slate-900 break-all select-all">
                      {walletQuery.data.address}
                    </span>
                    <CopyButton
                      text={walletQuery.data.address}
                      label="复制地址"
                      toastMessage="充值地址已复制到剪贴板"
                      className="shrink-0 self-start sm:self-center"
                    />
                  </div>
                </div>

                <div className="divide-y divide-slate-100 border-y border-slate-100">
                  <div className="py-2.5 flex items-center justify-between gap-3">
                    <span className="text-slate-500">目标网络</span>
                    <strong className="font-semibold text-slate-900">
                      {isBitcoin ? `Bitcoin ${btcNetworkLabel}` : 'Ethereum Sepolia'}
                    </strong>
                  </div>
                  {isBitcoin && (
                    <div className="py-2.5 flex items-center justify-between gap-3">
                      <span className="text-slate-500">地址类型</span>
                      <strong className="font-mono text-slate-900">P2WPKH</strong>
                    </div>
                  )}
                  {isToken && (
                    <>
                      <div className="py-2.5 flex items-center justify-between gap-3">
                        <span className="text-slate-500 shrink-0">Token 合约</span>
                        <AddressText value={asset.contract_address ?? ''} short />
                      </div>
                      <div className="py-2.5 flex items-center justify-between gap-3">
                        <span className="text-slate-500">精度 (Decimals)</span>
                        <strong className="font-mono text-slate-900">{asset.decimals}</strong>
                      </div>
                    </>
                  )}
                </div>

                <AlertBanner
                  type="warning"
                  title={`仅向该地址发送 ${
                    isBitcoin ? `Bitcoin ${btcNetworkLabel} BTC` : `Sepolia ${asset.symbol}`
                  }，使用其他网络转入资产可能导致永久丢失。`}
                />
              </div>
            </div>
          )}
        </Panel>
      </div>

      <Panel>
        <SectionHeader title={`${asset.symbol} 充值记录`} subtitle="点击记录行可查看详细入账参数" />
        {depositsQuery.isLoading ? (
          <TableSkeleton rows={5} cols={6} />
        ) : depositsQuery.error ? (
          <QueryState loading={false} error={depositsQuery.error} retry={() => depositsQuery.refetch()} />
        ) : isBitcoin ? (
          depositsQuery.data && 'items' in depositsQuery.data && depositsQuery.data.items.length > 0 ? (
            <div className="overflow-x-auto">
              <table className="w-full text-left border-collapse text-xs">
                <thead>
                  <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                    <th className="py-2.5 px-4">金额</th>
                    <th className="py-2.5 px-4">交易 ID</th>
                    <th className="py-2.5 px-4 text-right">输出 (vout)</th>
                    <th className="py-2.5 px-4 text-right">区块高度</th>
                    <th className="py-2.5 px-4 text-right">确认数</th>
                    <th className="py-2.5 px-4">状态</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-slate-100">
                  {(depositsQuery.data.items as BitcoinDeposit[]).map((row) => (
                    <tr
                      key={row.id}
                      onClick={() => setSelectedDetail(btcDepositToDetail(row))}
                      className="hover:bg-slate-50/80 transition-colors cursor-pointer"
                    >
                      <td className="py-3 px-4 font-mono font-semibold text-emerald-600 whitespace-nowrap">
                        +{row.amount_sats.toLocaleString()} sat BTC
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap">
                        <HashLink hash={row.txid} copyable />
                      </td>
                      <td className="py-3 px-4 text-right font-mono text-slate-600">{row.vout}</td>
                      <td className="py-3 px-4 text-right font-mono text-slate-600">
                        {row.block_height.toLocaleString()}
                      </td>
                      <td className="py-3 px-4 text-right font-mono text-slate-600">{row.confirmations}</td>
                      <td className="py-3 px-4 whitespace-nowrap">
                        <StatusBadge status={row.status} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <EmptyState description="尚未检测到 BTC 充值" />
          )
        ) : depositsQuery.data && depositsQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">金额</th>
                  <th className="py-2.5 px-4">交易哈希</th>
                  <th className="py-2.5 px-4 text-right">区块</th>
                  <th className="py-2.5 px-4 text-right">确认数</th>
                  <th className="py-2.5 px-4">状态</th>
                  <th className="py-2.5 px-4 text-right">时间</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {(depositsQuery.data.items as (Deposit | TokenDeposit)[]).map((row) => {
                  const amount = 'amount' in row ? row.amount : row.amount_eth
                  return (
                    <tr
                      key={row.id}
                      onClick={() => setSelectedDetail(evmDepositToDetail(row, asset.symbol))}
                      className="hover:bg-slate-50/80 transition-colors cursor-pointer"
                    >
                      <td className="py-3 px-4 font-mono font-semibold text-emerald-600 whitespace-nowrap">
                        +{formatETH(amount)} {asset.symbol}
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap">
                        <HashLink hash={row.tx_hash} url={row.explorer_url} />
                      </td>
                      <td className="py-3 px-4 text-right font-mono whitespace-nowrap">
                        <a
                          href={row.block_url}
                          target="_blank"
                          rel="noreferrer"
                          onClick={(e) => e.stopPropagation()}
                          className="text-blue-600 hover:underline"
                        >
                          {row.block_number.toLocaleString()}
                        </a>
                      </td>
                      <td className="py-3 px-4 text-right font-mono text-slate-600">{row.confirmations}</td>
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
        ) : (
          <EmptyState description={`尚未检测到 ${asset.symbol} 充值`} />
        )}
      </Panel>

      <TxDetailDrawer detail={selectedDetail} onClose={() => setSelectedDetail(null)} />
    </div>
  )
}
