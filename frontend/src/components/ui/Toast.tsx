import { AlertCircle, AlertTriangle, CheckCircle2, Info, X } from 'lucide-react'
import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from 'react'

type ToastType = 'success' | 'error' | 'info' | 'warning'

type ToastItem = {
  id: number
  type: ToastType
  message: string
}

type ToastContextValue = {
  show: (type: ToastType, message: string) => void
  success: (message: string) => void
  error: (message: string) => void
  info: (message: string) => void
  warning: (message: string) => void
}

const ToastContext = createContext<ToastContextValue | null>(null)

const TOAST_STYLES: Record<ToastType, { box: string; icon: ReactNode }> = {
  success: {
    box: 'bg-white border-emerald-200 text-slate-800',
    icon: <CheckCircle2 className="w-4 h-4 text-emerald-600 shrink-0" />,
  },
  error: {
    box: 'bg-white border-rose-200 text-slate-800',
    icon: <AlertCircle className="w-4 h-4 text-rose-600 shrink-0" />,
  },
  warning: {
    box: 'bg-white border-amber-200 text-slate-800',
    icon: <AlertTriangle className="w-4 h-4 text-amber-600 shrink-0" />,
  },
  info: {
    box: 'bg-white border-blue-200 text-slate-800',
    icon: <Info className="w-4 h-4 text-blue-600 shrink-0" />,
  },
}

let nextToastId = 1

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([])

  const dismiss = useCallback((id: number) => {
    setItems((prev) => prev.filter((item) => item.id !== id))
  }, [])

  const show = useCallback(
    (type: ToastType, message: string) => {
      const id = nextToastId++
      setItems((prev) => [...prev.slice(-3), { id, type, message }])
      window.setTimeout(() => {
        dismiss(id)
      }, 2600)
    },
    [dismiss],
  )

  const value = useMemo<ToastContextValue>(
    () => ({
      show,
      success: (message: string) => show('success', message),
      error: (message: string) => show('error', message),
      info: (message: string) => show('info', message),
      warning: (message: string) => show('warning', message),
    }),
    [show],
  )

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        className="fixed top-16 right-4 z-[60] flex flex-col gap-2 pointer-events-none max-w-sm w-full items-end"
      >
        {items.map((item) => {
          const style = TOAST_STYLES[item.type]
          return (
            <div
              key={item.id}
              className={`pointer-events-auto flex items-center gap-2.5 px-3.5 py-2.5 rounded-lg border shadow-lg text-xs font-medium transition-all ${style.box}`}
            >
              {style.icon}
              <span className="leading-snug">{item.message}</span>
              <button
                type="button"
                onClick={() => dismiss(item.id)}
                aria-label="关闭提示"
                className="ml-1 text-slate-400 hover:text-slate-600 cursor-pointer"
              >
                <X className="w-3.5 h-3.5" />
              </button>
            </div>
          )
        })}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext)
  if (!ctx) {
    return {
      show: () => {},
      success: () => {},
      error: () => {},
      info: () => {},
      warning: () => {},
    }
  }
  return ctx
}
