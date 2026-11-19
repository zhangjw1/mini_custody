import { useState } from 'react'
import { Eye, EyeOff, Lock, LogIn, ShieldCheck, User as UserIcon, WalletCards } from 'lucide-react'
import { Navigate, useNavigate } from 'react-router-dom'
import { ApiError } from '../api'
import { useAuth } from '../AuthContext'
import { AlertBanner } from '../components/Common'
import { Button } from '../components/ui/Button'
import { useToast } from '../components/ui/Toast'

export default function LoginPage() {
  const { isAuthenticated, isChecking, login } = useAuth()
  const navigate = useNavigate()
  const toast = useToast()

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [errorMessage, setErrorMessage] = useState<string | null>(null)
  const [fieldErrors, setFieldErrors] = useState<{ username?: string; password?: string }>({})

  if (!isChecking && isAuthenticated) {
    return <Navigate to="/" replace />
  }

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    const cleanUser = username.trim()
    const nextErrors: { username?: string; password?: string } = {}
    if (!cleanUser) nextErrors.username = '请输入管理员账号'
    if (!password) nextErrors.password = '请输入登录密码'
    setFieldErrors(nextErrors)
    if (Object.keys(nextErrors).length > 0) return

    setSubmitting(true)
    setErrorMessage(null)
    try {
      const admin = await login(cleanUser, password)
      toast.success(`欢迎回来，${admin.display_name || admin.username}`)
      navigate('/', { replace: true })
    } catch (err) {
      if (err instanceof ApiError) {
        setErrorMessage(err.message || '账号或密码错误')
      } else {
        setErrorMessage('登录服务不可用，请稍后重试')
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="min-h-screen w-full bg-[#f3f5f8] flex flex-col items-center justify-center p-4">
      <div className="w-full max-w-md space-y-5">
        {/* Brand Header */}
        <div className="flex flex-col items-center text-center space-y-2.5">
          <div className="w-11 h-11 rounded-xl bg-[#09182b] border border-slate-800 shadow-md flex items-center justify-center">
            <WalletCards className="w-5 h-5 text-cyan-400" />
          </div>
          <div>
            <h1 className="text-xl font-bold text-slate-900 tracking-tight">Mini Custody</h1>
            <p className="text-xs text-slate-500 mt-0.5">多链资产托管与链上对账管理员控制台</p>
          </div>
        </div>

        {/* White Card */}
        <div className="bg-white border border-slate-200 rounded-xl shadow-lg overflow-hidden">
          <div className="px-6 py-4 border-b border-slate-100 bg-slate-50/70 flex items-center justify-between">
            <span className="text-xs font-bold uppercase tracking-wider text-slate-800">
              管理员身份认证
            </span>
            <span className="inline-flex items-center gap-1 text-[11px] font-mono text-slate-500">
              <ShieldCheck className="w-3.5 h-3.5 text-emerald-600" />
              JWT Protected
            </span>
          </div>

          <form onSubmit={handleSubmit} className="p-6 space-y-4 text-xs" noValidate>
            {errorMessage && <AlertBanner type="error" title={errorMessage} />}

            <div className="space-y-1.5">
              <label htmlFor="login-username" className="block font-medium text-slate-700">
                管理员账号
              </label>
              <div className="relative">
                <UserIcon className="w-4 h-4 text-slate-400 absolute left-3 top-1/2 -translate-y-1/2 pointer-events-none" />
                <input
                  id="login-username"
                  type="text"
                  autoComplete="username"
                  placeholder="请输入用户名"
                  value={username}
                  onChange={(e) => {
                    setUsername(e.target.value)
                    if (fieldErrors.username) setFieldErrors((p) => ({ ...p, username: undefined }))
                  }}
                  className={`w-full h-10 pl-9 pr-3 rounded-md border text-xs bg-white transition-colors focus:outline-none focus:ring-1 ${
                    fieldErrors.username
                      ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                      : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
                  }`}
                />
              </div>
              {fieldErrors.username && (
                <p role="alert" className="text-[11px] text-rose-600 font-medium">
                  {fieldErrors.username}
                </p>
              )}
            </div>

            <div className="space-y-1.5">
              <label htmlFor="login-password" className="block font-medium text-slate-700">
                登录密码
              </label>
              <div className="relative">
                <Lock className="w-4 h-4 text-slate-400 absolute left-3 top-1/2 -translate-y-1/2 pointer-events-none" />
                <input
                  id="login-password"
                  type={showPassword ? 'text' : 'password'}
                  autoComplete="current-password"
                  placeholder="请输入密码"
                  value={password}
                  onChange={(e) => {
                    setPassword(e.target.value)
                    if (fieldErrors.password) setFieldErrors((p) => ({ ...p, password: undefined }))
                  }}
                  className={`w-full h-10 pl-9 pr-10 rounded-md border text-xs bg-white transition-colors focus:outline-none focus:ring-1 ${
                    fieldErrors.password
                      ? 'border-rose-400 focus:border-rose-500 focus:ring-rose-500'
                      : 'border-slate-200 focus:border-blue-500 focus:ring-blue-500'
                  }`}
                />
                <button
                  type="button"
                  onClick={() => setShowPassword((v) => !v)}
                  aria-label={showPassword ? '隐藏密码' : '显示密码'}
                  className="absolute right-2.5 top-1/2 -translate-y-1/2 p-1 text-slate-400 hover:text-slate-700 cursor-pointer"
                >
                  {showPassword ? <EyeOff className="w-4 h-4" /> : <Eye className="w-4 h-4" />}
                </button>
              </div>
              {fieldErrors.password && (
                <p role="alert" className="text-[11px] text-rose-600 font-medium">
                  {fieldErrors.password}
                </p>
              )}
            </div>

            <div className="pt-1">
              <Button
                type="submit"
                variant="primary"
                size="lg"
                block
                loading={submitting}
                icon={<LogIn className="w-4 h-4" />}
              >
                登录控制台
              </Button>
            </div>
          </form>
        </div>

        <div className="text-center text-[11px] text-slate-400 font-mono">
          Mini Custody Operator Console · Authorized Access Only
        </div>
      </div>
    </div>
  )
}
