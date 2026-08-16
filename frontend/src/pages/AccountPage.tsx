import { useQuery } from '@tanstack/react-query'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import { AddressText, formatDate, formatETH, PageHeader, QueryState, SectionHeader } from '../components/Common'

export default function AccountPage() {
  const { userId, users } = useAppContext()
  const user = users.find((item) => item.id === userId)
  const walletQuery = useQuery({ queryKey: ['wallet', userId], queryFn: () => api.wallet(userId), enabled: userId > 0 })

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
        {walletQuery.data && (
          <>
            <div className="wallet-address"><span>充值地址</span><AddressText value={walletQuery.data.address} /></div>
            <div className="balance-grid">
              <div><span>可用余额</span><strong>{formatETH(walletQuery.data.balance.available_eth)} <small>ETH</small></strong></div>
              <div><span>待确认充值</span><strong>{formatETH(walletQuery.data.balance.pending_deposit_eth)} <small>ETH</small></strong></div>
              <div><span>处理中提币</span><strong>{formatETH(walletQuery.data.balance.pending_withdrawal_eth)} <small>ETH</small></strong></div>
            </div>
          </>
        )}
      </section>
    </>
  )
}
