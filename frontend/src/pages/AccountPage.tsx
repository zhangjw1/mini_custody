import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Edit3, Power, UserPlus } from 'lucide-react'
import { api } from '../api'
import { useAppContext } from '../AppContext'
import {
  AddressText,
  AlertBanner,
  CopyButton,
  EmptyState,
  formatDate,
  formatETH,
  PageHeader,
  Panel,
  QueryState,
  SectionHeader,
  StatusBadge,
} from '../components/Common'
import { Button } from '../components/ui/Button'
import { TableSkeleton } from '../components/ui/Skeleton'
import { useToast } from '../components/ui/Toast'
import { EditUserModal } from '../components/UserModals'
import type { User } from '../types'

export default function AccountPage() {
  const { userId, setUserId, users, openCreateUserModal } = useAppContext()
  const queryClient = useQueryClient()
  const toast = useToast()

  const [editingUser, setEditingUser] = useState<User | null>(null)
  const user = users.find((item) => item.id === userId)

  const walletQuery = useQuery({
    queryKey: ['wallet', userId],
    queryFn: () => api.wallet(userId),
    enabled: userId > 0,
  })
  const balancesQuery = useQuery({
    queryKey: ['balances', userId],
    queryFn: () => api.balances(userId),
    enabled: userId > 0,
    refetchInterval: 10_000,
  })

  const toggleStatusMutation = useMutation({
    mutationFn: (target: User) =>
      api.updateUser(target.id, {
        display_name: target.display_name,
        note: target.note,
        status: target.status === 'DISABLED' ? 'ACTIVE' : 'DISABLED',
      }),
    onSuccess: async (updated) => {
      await queryClient.invalidateQueries({ queryKey: ['users'] })
      toast.success(`用户 ${updated.display_name} 已切换为 ${updated.status}`)
    },
    onError: (err) => {
      toast.error(err instanceof Error ? err.message : '状态切换失败')
    },
  })

  return (
    <div className="space-y-5">
      <PageHeader
        title="托管账户"
        subtitle="用户主体信息、状态管理、托管地址与多资产余额明细"
        extra={
          <Button
            variant="primary"
            size="sm"
            icon={<UserPlus className="w-3.5 h-3.5" />}
            onClick={openCreateUserModal}
          >
            新建托管用户
          </Button>
        }
      />

      {user?.status === 'DISABLED' && (
        <AlertBanner
          type="warning"
          title={`当前选中账户「${user.display_name}」处于 DISABLED（已禁用）状态，系统已限制其发起新的提币操作。`}
        />
      )}

      <Panel>
        <SectionHeader
          title="当前账户信息"
          extra={
            user && (
              <div className="flex items-center gap-2">
                <Button
                  variant="secondary"
                  size="xs"
                  icon={<Edit3 className="w-3 h-3" />}
                  onClick={() => setEditingUser(user)}
                >
                  编辑资料
                </Button>
                <Button
                  variant={user.status === 'DISABLED' ? 'primary' : 'danger'}
                  size="xs"
                  loading={toggleStatusMutation.isPending && toggleStatusMutation.variables?.id === user.id}
                  icon={<Power className="w-3 h-3" />}
                  onClick={() => toggleStatusMutation.mutate(user)}
                >
                  {user.status === 'DISABLED' ? '启用账户' : '禁用账户'}
                </Button>
              </div>
            )
          }
        />
        <div className="p-4 grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-5 gap-4 text-xs">
          <div className="space-y-1">
            <div className="text-[11px] text-slate-400">用户名称</div>
            <div className="font-semibold text-sm text-slate-900">{user?.display_name ?? '--'}</div>
          </div>
          <div className="space-y-1">
            <div className="text-[11px] text-slate-400">用户编码</div>
            <div className="font-mono font-semibold text-sm text-slate-900">{user?.code ?? '--'}</div>
          </div>
          <div className="space-y-1">
            <div className="text-[11px] text-slate-400">用户 ID</div>
            <div className="font-mono font-semibold text-sm text-slate-900">{user?.id ?? '--'}</div>
          </div>
          <div className="space-y-1">
            <div className="text-[11px] text-slate-400">账户状态</div>
            <div>
              {user ? <StatusBadge status={user.status ?? 'ACTIVE'} /> : <span className="text-slate-400">--</span>}
            </div>
          </div>
          <div className="space-y-1">
            <div className="text-[11px] text-slate-400">创建时间</div>
            <div className="font-mono font-medium text-sm text-slate-800">
              {user ? formatDate(user.created_at) : '--'}
            </div>
          </div>
          {user?.note && (
            <div className="sm:col-span-2 lg:col-span-5 pt-2 border-t border-slate-100 flex items-center gap-2 text-xs">
              <span className="text-slate-400 shrink-0">备注：</span>
              <span className="text-slate-700">{user.note}</span>
            </div>
          )}
        </div>
      </Panel>

      <Panel>
        <SectionHeader title="Sepolia 托管钱包" subtitle="所有 EVM 原生及 ERC-20 资产共用此托管地址" />
        <QueryState loading={walletQuery.isLoading} error={walletQuery.error} retry={() => walletQuery.refetch()} />
        {walletQuery.data && (
          <div className="p-4">
            <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3 p-3.5 rounded-lg border border-slate-200 bg-slate-50/70">
              <div className="min-w-0 space-y-1">
                <div className="text-[11px] font-medium text-slate-500">统一充值地址</div>
                <div className="font-mono text-xs sm:text-sm font-semibold text-slate-900 break-all">
                  {walletQuery.data.address}
                </div>
              </div>
              <CopyButton
                text={walletQuery.data.address}
                label="复制地址"
                toastMessage="托管钱包地址已复制"
                className="shrink-0 self-start sm:self-center"
              />
            </div>
          </div>
        )}
      </Panel>

      <Panel>
        <SectionHeader title="多资产余额" />
        {balancesQuery.isLoading ? (
          <TableSkeleton rows={4} cols={6} />
        ) : balancesQuery.error ? (
          <QueryState loading={false} error={balancesQuery.error} retry={() => balancesQuery.refetch()} />
        ) : balancesQuery.data && balancesQuery.data.items.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">资产</th>
                  <th className="py-2.5 px-4 text-right">可用余额</th>
                  <th className="py-2.5 px-4 text-right">待确认充值</th>
                  <th className="py-2.5 px-4 text-right">处理中提币</th>
                  <th className="py-2.5 px-4 text-right">精度</th>
                  <th className="py-2.5 px-4">合约地址</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {balancesQuery.data.items.map((row) => (
                  <tr key={row.asset_id} className="hover:bg-slate-50/70 transition-colors">
                    <td className="py-3 px-4 whitespace-nowrap">
                      <div className="inline-flex items-center gap-2">
                        <strong className="font-mono text-slate-900">{row.asset}</strong>
                        <span
                          className={`px-1.5 py-0.5 rounded text-[10px] font-bold font-mono border ${
                            row.asset_type === 'NATIVE'
                              ? 'bg-[#09182b] text-white border-[#09182b]'
                              : 'bg-slate-100 text-slate-600 border-slate-200'
                          }`}
                        >
                          {row.asset_type}
                        </span>
                      </div>
                    </td>
                    <td className="py-3 px-4 text-right font-mono font-bold text-slate-900 whitespace-nowrap">
                      {formatETH(row.available)} {row.asset}
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-600 whitespace-nowrap">
                      {formatETH(row.pending_deposit)} {row.asset}
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-600 whitespace-nowrap">
                      {formatETH(row.pending_withdrawal)} {row.asset}
                    </td>
                    <td className="py-3 px-4 text-right font-mono text-slate-500 whitespace-nowrap">
                      {row.decimals}
                    </td>
                    <td className="py-3 px-4 whitespace-nowrap">
                      {row.contract_address ? (
                        <AddressText value={row.contract_address} short />
                      ) : (
                        <span className="text-slate-400">原生资产</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState description="暂无余额记录" />
        )}
      </Panel>

      {/* All Custody Users Management Table */}
      <Panel>
        <SectionHeader
          title="托管用户管理"
          subtitle={`共 ${users.length} 个托管账户`}
          extra={
            <Button
              variant="secondary"
              size="xs"
              icon={<UserPlus className="w-3 h-3" />}
              onClick={openCreateUserModal}
            >
              新建用户
            </Button>
          }
        />
        {users.length > 0 ? (
          <div className="overflow-x-auto">
            <table className="w-full text-left border-collapse text-xs">
              <thead>
                <tr className="border-b border-slate-100 bg-slate-50/70 text-[11px] font-semibold text-slate-500">
                  <th className="py-2.5 px-4">ID</th>
                  <th className="py-2.5 px-4">编码</th>
                  <th className="py-2.5 px-4">显示名称</th>
                  <th className="py-2.5 px-4">状态</th>
                  <th className="py-2.5 px-4">备注</th>
                  <th className="py-2.5 px-4 text-right">创建时间</th>
                  <th className="py-2.5 px-4 text-right">操作</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-100">
                {users.map((item) => {
                  const isCurrent = item.id === userId
                  const isDisabled = item.status === 'DISABLED'
                  return (
                    <tr
                      key={item.id}
                      className={`transition-colors ${
                        isCurrent ? 'bg-blue-50/35' : 'hover:bg-slate-50/70'
                      }`}
                    >
                      <td className="py-3 px-4 font-mono text-slate-600 whitespace-nowrap">#{item.id}</td>
                      <td className="py-3 px-4 font-mono font-semibold text-slate-800 whitespace-nowrap">
                        {item.code}
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap">
                        <span className="font-semibold text-slate-900">{item.display_name}</span>
                        {isCurrent && (
                          <span className="ml-2 px-1.5 py-0.5 rounded bg-[#09182b] text-white text-[9px] font-mono">
                            当前
                          </span>
                        )}
                      </td>
                      <td className="py-3 px-4 whitespace-nowrap">
                        <StatusBadge status={item.status ?? 'ACTIVE'} />
                      </td>
                      <td className="py-3 px-4 text-slate-500 max-w-[200px] truncate" title={item.note}>
                        {item.note || '--'}
                      </td>
                      <td className="py-3 px-4 text-right font-mono text-slate-500 whitespace-nowrap">
                        {formatDate(item.created_at)}
                      </td>
                      <td className="py-3 px-4 text-right whitespace-nowrap">
                        <div className="inline-flex items-center gap-1.5">
                          {!isCurrent && (
                            <Button
                              variant="ghost"
                              size="xs"
                              onClick={() => setUserId(item.id)}
                            >
                              切换
                            </Button>
                          )}
                          <Button
                            variant="secondary"
                            size="xs"
                            icon={<Edit3 className="w-3 h-3" />}
                            onClick={() => setEditingUser(item)}
                          >
                            编辑
                          </Button>
                          <Button
                            variant={isDisabled ? 'primary' : 'ghost'}
                            size="xs"
                            loading={
                              toggleStatusMutation.isPending &&
                              toggleStatusMutation.variables?.id === item.id
                            }
                            onClick={() => toggleStatusMutation.mutate(item)}
                            className={!isDisabled ? 'text-rose-600 hover:bg-rose-50 hover:text-rose-700' : ''}
                          >
                            {isDisabled ? '启用' : '禁用'}
                          </Button>
                        </div>
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <EmptyState description="暂无托管用户，请点击右上角新建用户" />
        )}
      </Panel>

      <EditUserModal user={editingUser} onClose={() => setEditingUser(null)} />
    </div>
  )
}
