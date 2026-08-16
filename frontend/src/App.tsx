import { useEffect, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Button, Select, Tooltip } from 'antd'
import {
  ArrowDownToLine,
  ArrowUpFromLine,
  CircleUserRound,
  LayoutDashboard,
  RefreshCw,
  Rows3,
  WalletCards,
} from 'lucide-react'
import { NavLink, Navigate, Route, Routes } from 'react-router-dom'
import { api } from './api'
import { AppContext } from './AppContext'
import { StatusTag } from './components/Common'
import AccountPage from './pages/AccountPage'
import DashboardPage from './pages/DashboardPage'
import DepositPage from './pages/DepositPage'
import TransactionsPage from './pages/TransactionsPage'
import WithdrawPage from './pages/WithdrawPage'

const navigation = [
  { to: '/', label: '总览', icon: LayoutDashboard, end: true },
  { to: '/account', label: '账户', icon: CircleUserRound },
  { to: '/deposit', label: '充值', icon: ArrowDownToLine },
  { to: '/withdraw', label: '提币', icon: ArrowUpFromLine },
  { to: '/transactions', label: '流水', icon: Rows3 },
]

function Navigation({ mobile = false }: { mobile?: boolean }) {
  return (
    <nav className={mobile ? 'mobile-nav' : 'side-nav'} aria-label="主导航">
      {navigation.map(({ to, label, icon: Icon, end }) => (
        <NavLink key={to} to={to} end={end} className={({ isActive }) => isActive ? 'nav-item active' : 'nav-item'}>
          <Icon size={mobile ? 20 : 18} />
          <span>{label}</span>
        </NavLink>
      ))}
    </nav>
  )
}

export default function App() {
  const queryClient = useQueryClient()
  const [userId, setUserId] = useState(0)
  const usersQuery = useQuery({ queryKey: ['users'], queryFn: api.users })
  const chainQuery = useQuery({ queryKey: ['chain'], queryFn: api.chain, refetchInterval: 15_000 })
  const users = useMemo(() => usersQuery.data?.items ?? [], [usersQuery.data])

  useEffect(() => {
    if (!userId && users.length > 0) setUserId(users[0].id)
  }, [userId, users])

  const refreshAll = () => queryClient.invalidateQueries()

  return (
    <AppContext.Provider value={{ users, userId, setUserId }}>
      <div className="app-shell">
        <aside className="sidebar">
          <div className="brand">
            <span className="brand-mark"><WalletCards size={21} /></span>
            <span>Mini Custody</span>
          </div>
          <Navigation />
          <div className="sidebar-network">
            <span className="network-dot" />
            Ethereum Sepolia
          </div>
        </aside>

        <div className="app-main">
          <header className="topbar">
            <div className="mobile-brand">
              <span className="brand-mark"><WalletCards size={18} /></span>
              <strong>Mini Custody</strong>
            </div>
            <div className="topbar-controls">
              <Select
                aria-label="选择用户"
                className="user-select"
                value={userId || undefined}
                loading={usersQuery.isLoading}
                placeholder="选择用户"
                onChange={setUserId}
                options={users.map((user) => ({ value: user.id, label: `${user.display_name} · ${user.code}` }))}
              />
              <div className="chain-state">
                <span className="chain-label">Sepolia</span>
                <StatusTag status={chainQuery.data?.status ?? (chainQuery.isError ? 'DOWN' : 'CHECKING')} />
              </div>
              <Tooltip title="刷新全部数据">
                <Button type="text" icon={<RefreshCw size={17} />} onClick={refreshAll} aria-label="刷新全部数据" />
              </Tooltip>
            </div>
          </header>

          <main className="content">
            <Routes>
              <Route path="/" element={<DashboardPage />} />
              <Route path="/account" element={<AccountPage />} />
              <Route path="/deposit" element={<DepositPage />} />
              <Route path="/withdraw" element={<WithdrawPage />} />
              <Route path="/transactions" element={<TransactionsPage />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>
          </main>
        </div>
        <Navigation mobile />
      </div>
    </AppContext.Provider>
  )
}
