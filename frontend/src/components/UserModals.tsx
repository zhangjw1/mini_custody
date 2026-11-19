import { useEffect, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { CheckCircle2, UserPlus } from 'lucide-react'
import { api } from '../api'
import type { CreateUserResponse, User, UserStatus } from '../types'
import { AlertBanner, CopyButton } from './Common'
import { Button } from './ui/Button'
import { Modal } from './ui/Modal'
import { useToast } from './ui/Toast'

export function CreateUserModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated?: (res: CreateUserResponse) => void
}) {
  const queryClient = useQueryClient()
  const toast = useToast()

  const [code, setCode] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [note, setNote] = useState('')
  const [errors, setErrors] = useState<{ code?: string; display_name?: string }>({})
  const [createdResult, setCreatedResult] = useState<CreateUserResponse | null>(null)

  useEffect(() => {
    if (open) {
      setCode('')
      setDisplayName('')
      setNote('')
      setErrors({})
      setCreatedResult(null)
    }
  }, [open])

  const mutation = useMutation({
    mutationFn: api.createUser,
    onSuccess: async (res) => {
      setCreatedResult(res)
      await queryClient.invalidateQueries({ queryKey: ['users'] })
      onCreated?.(res)
      toast.success(`已创建托管用户 ${res.user.display_name}`)
    },
  })

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    const cleanCode = code.trim()
    const cleanName = displayName.trim()
    const cleanNote = note.trim()
    const nextErrors: { code?: string; display_name?: string } = {}

    if (!cleanCode) {
      nextErrors.code = '请输入用户编码（如 merchant-01）'
    } else if (!/^[a-zA-Z0-9_-]{2,64}$/.test(cleanCode)) {
      nextErrors.code = '编码仅支持 2-64 位字母、数字、下划线或短横线'
    }

    if (!cleanName) {
      nextErrors.display_name = '请输入用户显示名称'
    }

    setErrors(nextErrors)
    if (Object.keys(nextErrors).length > 0) return

    mutation.mutate({
      code: cleanCode,
      display_name: cleanName,
      ...(cleanNote ? { note: cleanNote } : {}),
    })
  }

  return (
    <Modal
      open={open}
      onClose={() => {
        if (!mutation.isPending) onClose()
      }}
      title={createdResult ? '托管用户创建成功' : '新建托管用户'}
      subtitle={
        createdResult
          ? `用户 #${createdResult.user.id} (${createdResult.user.code}) 已就绪`
          : '系统将自动为新用户分配专属 EVM 托管钱包地址'
      }
      footer={
        createdResult ? (
          <Button variant="primary" size="sm" onClick={onClose}>
            完成并切换至该用户
          </Button>
        ) : (
          <>
            <Button
              variant="secondary"
              size="sm"
              disabled={mutation.isPending}
              onClick={onClose}
            >
              取消
            </Button>
            <Button
              type="submit"
              form="create-user-form"
              variant="primary"
              size="sm"
              loading={mutation.isPending}
              icon={<UserPlus className="w-3.5 h-3.5" />}
            >
              确认创建
            </Button>
          </>
        )
      }
    >
      {createdResult ? (
        <div className="space-y-4">
          <div className="flex items-center gap-2.5 p-3 rounded-lg bg-emerald-50 border border-emerald-200 text-emerald-900">
            <CheckCircle2 className="w-4 h-4 text-emerald-600 shrink-0" />
            <span className="font-semibold">新托管账户及初始 EVM 地址已生成</span>
          </div>

          <div className="divide-y divide-slate-100 border border-slate-200 rounded-lg overflow-hidden">
            <div className="p-3 flex items-center justify-between gap-3">
              <span className="text-slate-500">用户 ID</span>
              <strong className="font-mono text-slate-900">#{createdResult.user.id}</strong>
            </div>
            <div className="p-3 flex items-center justify-between gap-3">
              <span className="text-slate-500">用户编码</span>
              <strong className="font-mono text-slate-900">{createdResult.user.code}</strong>
            </div>
            <div className="p-3 flex items-center justify-between gap-3">
              <span className="text-slate-500">显示名称</span>
              <strong className="text-slate-900">{createdResult.user.display_name}</strong>
            </div>
            <div className="p-3 space-y-1.5 bg-slate-50/70">
              <div className="flex items-center justify-between">
                <span className="text-slate-500">初始 EVM 托管地址</span>
                <CopyButton
                  text={createdResult.address}
                  label="复制地址"
                  toastMessage="初始 EVM 地址已复制"
                />
              </div>
              <div className="font-mono text-xs font-semibold text-slate-900 break-all select-all">
                {createdResult.address}
              </div>
            </div>
          </div>
        </div>
      ) : (
        <form id="create-user-form" onSubmit={handleSubmit} className="space-y-3.5" noValidate>
          {mutation.isError && <AlertBanner type="error" title={mutation.error.message} />}

          <div className="space-y-1.5">
            <label htmlFor="new-user-code" className="block font-medium text-slate-700">
              用户编码 <span className="text-rose-500">*</span>
            </label>
            <input
              id="new-user-code"
              type="text"
              placeholder="例如 merchant-001"
              value={code}
              onChange={(e) => {
                setCode(e.target.value)
                if (errors.code) setErrors((p) => ({ ...p, code: undefined }))
              }}
              className={`w-full h-9 px-3 rounded-md border font-mono text-xs bg-white focus:outline-none focus:ring-1 ${
                errors.code
                  ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                  : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
              }`}
            />
            {errors.code && <p className="text-[11px] text-rose-600">{errors.code}</p>}
          </div>

          <div className="space-y-1.5">
            <label htmlFor="new-user-name" className="block font-medium text-slate-700">
              显示名称 <span className="text-rose-500">*</span>
            </label>
            <input
              id="new-user-name"
              type="text"
              placeholder="例如 做市账户 A"
              value={displayName}
              onChange={(e) => {
                setDisplayName(e.target.value)
                if (errors.display_name) setErrors((p) => ({ ...p, display_name: undefined }))
              }}
              className={`w-full h-9 px-3 rounded-md border text-xs bg-white focus:outline-none focus:ring-1 ${
                errors.display_name
                  ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                  : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
              }`}
            />
            {errors.display_name && (
              <p className="text-[11px] text-rose-600">{errors.display_name}</p>
            )}
          </div>

          <div className="space-y-1.5">
            <label htmlFor="new-user-note" className="block font-medium text-slate-700">
              备注 <span className="text-slate-400 font-normal">(可选)</span>
            </label>
            <textarea
              id="new-user-note"
              rows={2}
              placeholder="业务线、用途说明等"
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="w-full px-3 py-2 rounded-md border border-slate-200 text-xs bg-white focus:outline-none focus:ring-1 focus:border-blue-500 focus:ring-blue-500 resize-none"
            />
          </div>
        </form>
      )}
    </Modal>
  )
}

export function EditUserModal({
  user,
  onClose,
}: {
  user: User | null
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const toast = useToast()

  const [displayName, setDisplayName] = useState('')
  const [note, setNote] = useState('')
  const [status, setStatus] = useState<UserStatus>('ACTIVE')
  const [nameError, setNameError] = useState<string | null>(null)

  useEffect(() => {
    if (user) {
      setDisplayName(user.display_name)
      setNote(user.note ?? '')
      setStatus(user.status ?? 'ACTIVE')
      setNameError(null)
    }
  }, [user])

  const mutation = useMutation({
    mutationFn: (input: { display_name: string; note?: string; status: UserStatus }) =>
      api.updateUser(user!.id, input),
    onSuccess: async (updated) => {
      await queryClient.invalidateQueries({ queryKey: ['users'] })
      toast.success(`已更新用户 ${updated.display_name} (${updated.status})`)
      onClose()
    },
  })

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!user) return
    const cleanName = displayName.trim()
    if (!cleanName) {
      setNameError('请输入显示名称')
      return
    }
    setNameError(null)
    mutation.mutate({
      display_name: cleanName,
      note: note.trim() || undefined,
      status,
    })
  }

  return (
    <Modal
      open={Boolean(user)}
      onClose={() => {
        if (!mutation.isPending) onClose()
      }}
      title="编辑托管用户"
      subtitle={user ? `用户 #${user.id} · ${user.code}` : undefined}
      footer={
        <>
          <Button
            variant="secondary"
            size="sm"
            disabled={mutation.isPending}
            onClick={onClose}
          >
            取消
          </Button>
          <Button
            type="submit"
            form="edit-user-form"
            variant="primary"
            size="sm"
            loading={mutation.isPending}
          >
            保存修改
          </Button>
        </>
      }
    >
      {user && (
        <form id="edit-user-form" onSubmit={handleSubmit} className="space-y-3.5" noValidate>
          {mutation.isError && <AlertBanner type="error" title={mutation.error.message} />}

          <div className="space-y-1.5">
            <label htmlFor="edit-user-name" className="block font-medium text-slate-700">
              显示名称 <span className="text-rose-500">*</span>
            </label>
            <input
              id="edit-user-name"
              type="text"
              value={displayName}
              onChange={(e) => {
                setDisplayName(e.target.value)
                if (nameError) setNameError(null)
              }}
              className={`w-full h-9 px-3 rounded-md border text-xs bg-white focus:outline-none focus:ring-1 ${
                nameError
                  ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                  : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
              }`}
            />
            {nameError && <p className="text-[11px] text-rose-600">{nameError}</p>}
          </div>

          <div className="space-y-1.5">
            <span className="block font-medium text-slate-700">账户状态</span>
            <div className="grid grid-cols-2 gap-2">
              {(['ACTIVE', 'DISABLED'] as const).map((opt) => {
                const selected = status === opt
                return (
                  <button
                    key={opt}
                    type="button"
                    onClick={() => setStatus(opt)}
                    className={`py-2 px-3 rounded-md border text-xs font-mono font-semibold transition-colors cursor-pointer ${
                      selected
                        ? opt === 'ACTIVE'
                          ? 'border-emerald-500 bg-emerald-50 text-emerald-700'
                          : 'border-rose-500 bg-rose-50 text-rose-700'
                        : 'border-slate-200 bg-white text-slate-600 hover:bg-slate-50'
                    }`}
                  >
                    {opt === 'ACTIVE' ? 'ACTIVE · 启用' : 'DISABLED · 禁用'}
                  </button>
                )
              })}
            </div>
            {status === 'DISABLED' && (
              <p className="text-[11px] text-amber-700">
                禁用后该用户将被标记为 DISABLED，禁止在控制台发起新的提币。
              </p>
            )}
          </div>

          <div className="space-y-1.5">
            <label htmlFor="edit-user-note" className="block font-medium text-slate-700">
              备注
            </label>
            <textarea
              id="edit-user-note"
              rows={2}
              placeholder="可选备注信息"
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="w-full px-3 py-2 rounded-md border border-slate-200 text-xs bg-white focus:outline-none focus:ring-1 focus:border-blue-500 focus:ring-blue-500 resize-none"
            />
          </div>
        </form>
      )}
    </Modal>
  )
}
