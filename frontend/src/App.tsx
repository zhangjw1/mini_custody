import { useEffect, useMemo, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  ArrowDownToLine,
  ArrowUpFromLine,
  Check,
  ChevronDown,
  CircleUserRound,
  LayoutDashboard,
  LoaderCircle,
  LogOut,
  RefreshCw,
  Rows3,
  Settings2,
  Shield,
  UserPlus,
  WalletCards,
} from 'lucide-react'
import { NavLink, Navigate, Route, Routes, useNavigate } from 'react-router-dom'
import { api } from './api'
import { AppContext } from './AppContext'
import { useAuth } from './AuthContext'
import { FooterBar } from './components/FooterBar'
import { StatusBadge } from './components/ui/StatusBadge'
import { useToast } from './components/ui/Toast'
import { CreateUserModal } from './components/UserModals'
import AccountPage from './pages/AccountPage'
import DashboardPage from './pages/DashboardPage'
import DepositPage from './pages/DepositPage'
import LoginPage from './pages/LoginPage'
import OperationsPage from './pages/OperationsPage'
import TransactionsPage from './pages/TransactionsPage'
import WithdrawPage from './pages/WithdrawPage'

const navigation = [
  { to: '/', label: '总览', icon: LayoutDashboard, end: true },
  { to: '/account', label: '账户', icon: CircleUserRound },
  { to: '/deposit', label: '充值', icon: ArrowDownToLine },
  { to: '/withdraw', label: '提币', icon: ArrowUpFromLine },
  { to: '/transactions', label: '流水', icon: Rows3 },
  { to: '/operations', label: '运维', icon: Settings2 },
]

function formatNetworkLabel(network: string): { name: string; badge: string } {
  if (network === 'bitcoin-signet') return { name: 'Bitcoin Signet', badge: 'BTC' }
  if (network === 'bitcoin-testnet4') return { name: 'Bitcoin Testnet4', badge: 'BTC' }
  if (network === 'ethereum-sepolia' || network === 'sepolia') {
    return { name: 'Ethereum Sepolia', badge: 'EVM' }
  }
  return { name: network || 'Unknown Network', badge: 'CHAIN' }
}

function ProtectedShell() {
  const { admin, isAuthenticated, isChecking, logout } = useAuth()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const toast = useToast()

  const [userId, setUserId] = useState(0)
  const [assetSymbol, setAssetSymbolState] = useState(
    () => localStorage.getItem('mini-custody-asset') ?? 'ETH',
  )
  const [assetMenuOpen, setAssetMenuOpen] = useState(false)
  const [userMenuOpen, setUserMenuOpen] = useState(false)
  const [createUserOpen, setCreateUserOpen] = useState(false)
  const [refreshing, setRefreshing] = useState(false)

  const usersQuery = useQuery({
    queryKey: ['users'],
    queryFn: api.users,
    enabled: isAuthenticated,
  })
  const assetsQuery = useQuery({
    queryKey: ['assets'],
    queryFn: api.assets,
    enabled: isAuthenticated,
  })
  const chainQuery = useQuery({
    queryKey: ['chain'],
    queryFn: api.chain,
    refetchInterval: 15_000,
    enabled: isAuthenticated,
  })

  const users = useMemo(() => usersQuery.data?.items ?? [], [usersQuery.data])
  const assets = useMemo(
    () => (assetsQuery.data?.items ?? []).filter((item) => item.enabled),
    [assetsQuery.data],
  )
  const asset =
    assets.find((item) => item.symbol === assetSymbol) ??
    assets[0] ?? {
      id: 0,
      network: 'ethereum-sepolia',
      asset_type: 'NATIVE' as const,
      symbol: 'ETH',
      decimals: 18,
      enabled: true,
      updated_at: '',
    }

  const currentUser = users.find((u) => u.id === userId)

  useEffect(() => {
    if (!userId && users.length > 0) setUserId(users[0].id)
  }, [userId, users])

  useEffect(() => {
    if (assets.length > 0 && !assets.some((item) => item.symbol === assetSymbol)) {
      setAssetSymbolState(assets[0].symbol)
    }
  }, [assetSymbol, assets])

  if (isChecking) {
    return (
      <div className="min-h-screen w-screen bg-[#f3f5f8] flex flex-col items-center justify-center gap-3 text-xs text-slate-500">
        <LoaderCircle className="w-6 h-6 text-[#09182b] animate-spin" />
        <span className="font-mono">正在验证管理员会话...</span>
      </div>
    )
  }

  if (!isAuthenticated) {
    return <Navigate to="/login" replace />
  }

  const setAssetSymbol = (symbol: string) => {
    localStorage.setItem('mini-custody-asset', symbol)
    setAssetSymbolState(symbol)
  }

  const refreshAll = async () => {
    setRefreshing(true)
    try {
      await queryClient.invalidateQueries()
      toast.info('已刷新全部链上与账户数据')
    } finally {
      window.setTimeout(() => setRefreshing(false), 400)
    }
  }

  const handleLogout = () => {
    logout()
    toast.info('已退出管理员登录')
    navigate('/login', { replace: true })
  }

  const chainStatus = chainQuery.data?.status ?? (chainQuery.isError ? 'DOWN' : 'CHECKING')
  const isHealthy = chainStatus === 'HEALTHY'
  const isDegraded = chainStatus === 'DEGRADED'
  const isDown = chainStatus === 'DOWN'
  const networkInfo = formatNetworkLabel(asset.network)

  const scannerLabel = isHealthy
    ? 'Indexer 同步中'
    : isDegraded
      ? 'Indexer 降级'
      : isDown
        ? 'Indexer 异常'
        : 'Indexer 检查中'

  return (
    <AppContext.Provider
      value={{
        users,
        userId,
        setUserId,
        currentUser,
        assets,
        asset,
        setAssetSymbol,
        capabilities: chainQuery.data?.capabilities,
        openCreateUserModal: () => setCreateUserOpen(true),
      }}
    >
      <div className="flex flex-col h-screen w-screen overflow-hidden bg-[#f3f5f8] text-slate-800">
        {/* Anchor-style Top Header */}
        <header className="h-14 bg-white border-b border-slate-200 px-4 md:px-5 flex items-center justify-between shrink-0 z-30 select-none">
          {/* Brand */}
          <div className="flex items-center gap-3 min-w-0">
            <div className="w-8 h-8 rounded-lg bg-[#09182b] flex items-center justify-center shadow-xs border border-slate-800 shrink-0">
              <WalletCards className="w-4 h-4 text-cyan-400" />
            </div>
            <span className="text-base md:text-lg font-bold text-slate-900 tracking-tight whitespace-nowrap">
              Mini Custody
            </span>
            <span className="text-slate-300 font-light hidden sm:inline">|</span>
            <span className="text-xs text-slate-500 font-normal tracking-wide hidden xl:inline truncate">
              多链资产托管、链上扫描与资金对账
            </span>
          </div>

          {/* Right Controls */}
          <div className="flex items-center gap-2 sm:gap-3">
            {/* Indexer Pulse Status */}
            <div
              className="hidden sm:flex items-center gap-1.5 text-xs font-medium text-slate-700"
              title={chainQuery.data?.last_error || `Chain status: ${chainStatus}`}
            >
              <span
                className={`w-2 h-2 rounded-full shrink-0 ${
                  isHealthy
                    ? 'bg-emerald-500 animate-pulse'
                    : isDegraded
                      ? 'bg-amber-500 animate-pulse'
                      : isDown
                        ? 'bg-rose-500'
                        : 'bg-blue-500 animate-pulse'
                }`}
              />
              <span className={isDown ? 'text-rose-600 font-semibold' : ''}>{scannerLabel}</span>
            </div>

            {/* Asset Selector Pill */}
            <div className="relative">
              <button
                type="button"
                aria-label="选择资产"
                onClick={() => {
                  setAssetMenuOpen(!assetMenuOpen)
                  setUserMenuOpen(false)
                }}
                className="flex items-center gap-1.5 bg-white hover:bg-slate-50 border border-slate-200 text-slate-800 text-xs font-semibold px-2.5 py-1.5 rounded-md shadow-2xs transition-colors cursor-pointer"
              >
                <span className="w-1.5 h-1.5 rounded-full bg-[#09182b] shrink-0" />
                <span className="font-mono">{asset.symbol}</span>
                <ChevronDown
                  className={`w-3.5 h-3.5 text-slate-400 transition-transform ${assetMenuOpen ? 'rotate-180' : ''}`}
                />
              </button>

              {assetMenuOpen && (
                <>
                  <div className="fixed inset-0 z-40" onClick={() => setAssetMenuOpen(false)} />
                  <div className="absolute right-0 mt-1.5 w-52 bg-white border border-slate-200 rounded-lg shadow-lg z-50 overflow-hidden text-xs">
                    <div className="px-3 py-2 border-b border-slate-100 bg-slate-50/80 text-[10px] font-bold text-slate-500 uppercase tracking-wider">
                      切换当前资产
                    </div>
                    <div className="py-1 max-h-64 overflow-y-auto">
                      {assets.map((item) => {
                        const active = item.symbol === asset.symbol
                        return (
                          <button
                            key={item.id || item.symbol}
                            type="button"
                            onClick={() => {
                              setAssetSymbol(item.symbol)
                              setAssetMenuOpen(false)
                            }}
                            className={`w-full text-left px-3 py-2 flex items-center gap-2 transition-colors ${
                              active ? 'bg-slate-50 text-slate-900' : 'text-slate-700 hover:bg-slate-50'
                            }`}
                          >
                            <span
                              className={`w-1.5 h-1.5 rounded-full shrink-0 ${active ? 'bg-[#09182b]' : 'bg-slate-300'}`}
                            />
                            <span className="flex-1 min-w-0">
                              <span className="block font-mono font-semibold">{item.symbol}</span>
                              <span className="block text-[10px] text-slate-400 truncate">
                                {item.network} · {item.asset_type}
                              </span>
                            </span>
                            {active && <Check className="w-3.5 h-3.5 text-[#09182b] shrink-0" />}
                          </button>
                        )
                      })}
                    </div>
                  </div>
                </>
              )}
            </div>

            {/* User Selector Pill */}
            <div className="relative">
              <button
                type="button"
                aria-label="选择用户"
                onClick={() => {
                  setUserMenuOpen(!userMenuOpen)
                  setAssetMenuOpen(false)
                }}
                className="flex items-center gap-1.5 bg-white hover:bg-slate-50 border border-slate-200 text-slate-800 text-xs font-medium px-2.5 py-1.5 rounded-md shadow-2xs transition-colors cursor-pointer max-w-[165px] sm:max-w-[230px]"
              >
                <CircleUserRound
                  className={`w-3.5 h-3.5 shrink-0 ${
                    currentUser?.status === 'DISABLED' ? 'text-rose-500' : 'text-slate-500'
                  }`}
                />
                <span className="truncate">
                  {currentUser
                    ? `${currentUser.display_name} · ${currentUser.code}`
                    : usersQuery.isLoading
                      ? '加载用户...'
                      : '暂无用户'}
                </span>
                {currentUser?.status === 'DISABLED' && (
                  <span className="px-1 py-0.2 rounded bg-rose-50 text-rose-600 border border-rose-200 text-[9px] font-mono font-bold shrink-0">
                    禁用
                  </span>
                )}
                <ChevronDown
                  className={`w-3.5 h-3.5 text-slate-400 shrink-0 transition-transform ${userMenuOpen ? 'rotate-180' : ''}`}
                />
              </button>

              {userMenuOpen && (
                <>
                  <div className="fixed inset-0 z-40" onClick={() => setUserMenuOpen(false)} />
                  <div className="absolute right-0 mt-1.5 w-64 bg-white border border-slate-200 rounded-lg shadow-lg z-50 overflow-hidden text-xs">
                    <div className="px-3 py-2 border-b border-slate-100 bg-slate-50/80 flex items-center justify-between">
                      <span className="text-[10px] font-bold text-slate-500 uppercase tracking-wider">
                        切换托管账户
                      </span>
                      <span className="text-[10px] font-mono text-slate-400">{users.length} 个</span>
                    </div>
                    <div className="py-1 max-h-64 overflow-y-auto">
                      {users.length === 0 ? (
                        <div className="px-3 py-4 text-center text-slate-400">暂无托管用户</div>
                      ) : (
                        users.map((u) => {
                          const active = u.id === userId
                          const disabled = u.status === 'DISABLED'
                          return (
                            <button
                              key={u.id}
                              type="button"
                              onClick={() => {
                                setUserId(u.id)
                                setUserMenuOpen(false)
                              }}
                              className={`w-full text-left px-3 py-2 flex items-center gap-2 transition-colors ${
                                active ? 'bg-slate-50 text-slate-900' : 'text-slate-700 hover:bg-slate-50'
                              }`}
                            >
                              <span className="flex-1 min-w-0">
                                <span className="flex items-center gap-1.5">
                                  <span className={`truncate ${active ? 'font-semibold' : 'font-medium'}`}>
                                    {u.display_name}
                                  </span>
                                  {disabled && (
                                    <span className="px-1 rounded bg-rose-50 text-rose-600 border border-rose-200 text-[9px] font-mono font-bold shrink-0">
                                      DISABLED
                                    </span>
                                  )}
                                </span>
                                <span className="block text-[10px] text-slate-400 font-mono">
                                  #{u.id} · {u.code}
                                </span>
                              </span>
                              {active && <Check className="w-3.5 h-3.5 text-[#09182b] shrink-0" />}
                            </button>
                          )
                        })
                      )}
                    </div>
                    <div className="p-2 border-t border-slate-100 bg-slate-50/50">
                      <button
                        type="button"
                        onClick={() => {
                          setUserMenuOpen(false)
                          setCreateUserOpen(true)
                        }}
                        className="w-full py-1.5 px-2.5 rounded-md bg-[#09182b] hover:bg-[#122640] text-white text-xs font-medium flex items-center justify-center gap-1.5 cursor-pointer"
                      >
                        <UserPlus className="w-3.5 h-3.5" />
                        <span>新建托管用户</span>
                      </button>
                    </div>
                  </div>
                </>
              )}
            </div>

            {/* Chain Metrics (Monospace) */}
            <div className="text-[11px] leading-tight text-slate-500 font-mono text-right hidden xl:block pl-2 border-l border-slate-200">
              <div>
                落后块数:{' '}
                <span
                  className={`font-semibold ${
                    chainQuery.data && chainQuery.data.lag > 5 ? 'text-amber-600' : 'text-slate-700'
                  }`}
                >
                  {chainQuery.data ? chainQuery.data.lag : '--'}
                </span>
              </div>
              <div>
                最新高度:{' '}
                <span className="font-semibold text-slate-700">
                  {chainQuery.data ? chainQuery.data.network_height.toLocaleString() : '--'}
                </span>
              </div>
            </div>

            {/* Refresh Button */}
            <button
              type="button"
              onClick={() => void refreshAll()}
              aria-label="刷新全部数据"
              title="刷新全部数据"
              className="w-8 h-8 rounded-md border border-slate-200 bg-white hover:bg-slate-50 text-slate-600 hover:text-slate-900 flex items-center justify-center transition-colors cursor-pointer shrink-0"
            >
              <RefreshCw className={`w-3.5 h-3.5 ${refreshing ? 'animate-spin text-blue-600' : ''}`} />
            </button>

            {/* Admin Name & Logout */}
            <div className="flex items-center gap-1.5 pl-2 border-l border-slate-200 shrink-0">
              <div
                className="hidden md:flex items-center gap-1.5 px-2.5 py-1 rounded-md bg-slate-50 border border-slate-200 text-xs"
                title={`当前管理员：${admin?.username ?? ''} (${admin?.role ?? ''})`}
              >
                <Shield className="w-3.5 h-3.5 text-[#09182b] shrink-0" />
                <span className="font-semibold text-slate-800 max-w-[110px] truncate">
                  {admin?.display_name || admin?.username || 'Admin'}
                </span>
              </div>

              <button
                type="button"
                onClick={handleLogout}
                aria-label="退出登录"
                title="退出登录"
                className="h-8 px-2.5 rounded-md border border-slate-200 bg-white hover:bg-rose-50 hover:border-rose-200 text-slate-600 hover:text-rose-600 flex items-center gap-1 text-xs font-medium transition-colors cursor-pointer shrink-0"
              >
                <LogOut className="w-3.5 h-3.5" />
                <span className="hidden sm:inline">退出</span>
              </button>
            </div>
          </div>
        </header>

        {/* Body Layout */}
        <div className="flex-1 flex overflow-hidden min-h-0">
          {/* Left Sidebar (240px) */}
          <aside className="hidden md:flex flex-col w-[240px] shrink-0 bg-white border-r border-slate-200 justify-between">
            <div className="p-3 space-y-4">
              <div>
                <div className="px-2.5 pb-2 text-[10px] font-bold uppercase tracking-wider text-slate-400">
                  导航菜单
                </div>
                <nav className="space-y-1" aria-label="主导航">
                  {navigation.map(({ to, label, icon: Icon, end }) => (
                    <NavLink
                      key={to}
                      to={to}
                      end={end}
                      className={({ isActive }) =>
                        `flex items-center gap-2.5 px-3 py-2 rounded-md text-xs font-medium transition-colors ${
                          isActive
                            ? 'bg-[#09182b] text-white shadow-2xs'
                            : 'text-slate-600 hover:bg-slate-100/80 hover:text-slate-900'
                        }`
                      }
                    >
                      {({ isActive }) => (
                        <>
                          <Icon
                            className={`w-4 h-4 shrink-0 ${isActive ? 'text-cyan-400' : 'text-slate-400'}`}
                          />
                          <span>{label}</span>
                        </>
                      )}
                    </NavLink>
                  ))}
                </nav>
              </div>

              <div className="pt-2 border-t border-slate-100">
                <button
                  type="button"
                  onClick={() => setCreateUserOpen(true)}
                  className="w-full flex items-center justify-center gap-1.5 px-3 py-2 rounded-md border border-slate-200 bg-slate-50/70 hover:bg-slate-100 text-slate-700 hover:text-slate-900 text-xs font-semibold transition-colors cursor-pointer"
                >
                  <UserPlus className="w-3.5 h-3.5 text-[#09182b]" />
                  <span>新建托管用户</span>
                </button>
              </div>
            </div>

            {/* Dynamic Network Status Card */}
            <div className="p-3 border-t border-slate-100">
              <div className="rounded-lg border border-slate-200 bg-slate-50/70 p-3 space-y-2 text-xs">
                <div className="flex items-center justify-between gap-2">
                  <span className="text-[10px] font-bold uppercase tracking-wider text-slate-400">
                    当前链网络
                  </span>
                  <span className="px-1.5 py-0.5 rounded bg-[#09182b] text-white text-[9px] font-bold font-mono tracking-wider">
                    {networkInfo.badge}
                  </span>
                </div>

                <div className="flex items-center gap-2">
                  <span
                    className={`w-2 h-2 rounded-full shrink-0 ${
                      isHealthy
                        ? 'bg-emerald-500'
                        : isDegraded
                          ? 'bg-amber-500'
                          : isDown
                            ? 'bg-rose-500'
                            : 'bg-blue-500 animate-pulse'
                    }`}
                  />
                  <span className="font-semibold text-slate-800 truncate">{networkInfo.name}</span>
                </div>

                <div className="pt-1.5 border-t border-slate-200/80 space-y-1 text-[11px] font-mono text-slate-500">
                  <div className="flex items-center justify-between">
                    <span>当前资产</span>
                    <span className="font-semibold text-slate-700">{asset.symbol}</span>
                  </div>
                  <div className="flex items-center justify-between">
                    <span>Chain ID</span>
                    <span className="font-semibold text-slate-700">
                      {asset.network.startsWith('bitcoin-')
                        ? asset.network
                        : (chainQuery.data?.chain_id ?? '--')}
                    </span>
                  </div>
                  <div className="flex items-center justify-between">
                    <span>扫描高度</span>
                    <span className="font-semibold text-slate-700">
                      {chainQuery.data ? chainQuery.data.scan_height.toLocaleString() : '--'}
                    </span>
                  </div>
                  {chainQuery.data?.capabilities && (
                    <div className="flex items-center justify-between pt-1">
                      <span>能力</span>
                      <span className="flex items-center gap-1">
                        <StatusBadge
                          status={chainQuery.data.capabilities.erc20_enabled ? 'ERC20' : 'NO-ERC20'}
                          showDot={false}
                          className="!px-1.5 !py-0 !text-[9px]"
                        />
                        <StatusBadge
                          status={chainQuery.data.capabilities.bitcoin_enabled ? 'BTC' : 'NO-BTC'}
                          showDot={false}
                          className="!px-1.5 !py-0 !text-[9px]"
                        />
                      </span>
                    </div>
                  )}
                </div>
              </div>
            </div>
          </aside>

          {/* Main Scrollable Viewport + FooterBar */}
          <div className="flex-1 flex flex-col min-w-0 min-h-0 overflow-hidden">
            <main className="flex-1 overflow-y-auto p-4 md:p-6 pb-20 md:pb-6">
              <div className="max-w-[1360px] w-full mx-auto">
                <Routes>
                  <Route path="/" element={<DashboardPage />} />
                  <Route path="/account" element={<AccountPage />} />
                  <Route path="/deposit" element={<DepositPage />} />
                  <Route path="/withdraw" element={<WithdrawPage />} />
                  <Route path="/transactions" element={<TransactionsPage />} />
                  <Route path="/operations" element={<OperationsPage />} />
                  <Route path="*" element={<Navigate to="/" replace />} />
                </Routes>
              </div>
            </main>

            <div className="hidden md:block">
              <FooterBar
                chain={chainQuery.data}
                isLoading={chainQuery.isLoading}
                isError={chainQuery.isError}
              />
            </div>
          </div>
        </div>

        {/* Mobile Bottom Navigation */}
        <nav
          className="md:hidden fixed bottom-0 inset-x-0 z-30 grid grid-cols-6 bg-white border-t border-slate-200 h-16 px-1"
          aria-label="移动端导航"
        >
          {navigation.map(({ to, label, icon: Icon, end }) => (
            <NavLink
              key={to}
              to={to}
              end={end}
              className={({ isActive }) =>
                `flex flex-col items-center justify-center gap-1 text-[10px] font-medium transition-colors ${
                  isActive ? 'text-[#09182b] font-bold' : 'text-slate-400 hover:text-slate-700'
                }`
              }
            >
              <Icon className="w-4 h-4" />
              <span>{label}</span>
            </NavLink>
          ))}
        </nav>
      </div>

      <CreateUserModal
        open={createUserOpen}
        onClose={() => setCreateUserOpen(false)}
        onCreated={(res) => setUserId(res.user.id)}
      />
    </AppContext.Provider>
  )
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/*" element={<ProtectedShell />} />
    </Routes>
  )
}
