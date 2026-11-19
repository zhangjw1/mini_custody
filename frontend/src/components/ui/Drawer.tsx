import { X } from 'lucide-react'
import { useEffect, type ReactNode } from 'react'

type DrawerProps = {
  open: boolean
  onClose: () => void
  title?: ReactNode
  subtitle?: ReactNode
  badge?: ReactNode
  footer?: ReactNode
  widthClass?: string
  children: ReactNode
}

export function Drawer({
  open,
  onClose,
  title,
  subtitle,
  badge,
  footer,
  widthClass = 'max-w-lg w-full',
  children,
}: DrawerProps) {
  useEffect(() => {
    if (!open) return
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handleKey)
    return () => window.removeEventListener('keydown', handleKey)
  }, [open, onClose])

  if (!open) return null

  return (
    <div
      className="fixed inset-0 z-50 flex justify-end bg-slate-900/35 backdrop-blur-xs"
      role="dialog"
      aria-modal="true"
      onClick={onClose}
    >
      <div
        className={`bg-white h-full ${widthClass} shadow-2xl border-l border-slate-200 flex flex-col`}
        onClick={(e) => e.stopPropagation()}
      >
        <div className="px-5 py-4 border-b border-slate-100 bg-slate-50/70 flex items-start justify-between gap-3 shrink-0">
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2 flex-wrap">
              {title && <h3 className="font-bold text-slate-900 text-sm">{title}</h3>}
              {badge}
            </div>
            {subtitle && <p className="text-[11px] text-slate-500 mt-0.5 font-mono truncate">{subtitle}</p>}
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label="关闭抽屉"
            className="w-7 h-7 rounded-md flex items-center justify-center text-slate-400 hover:text-slate-700 hover:bg-slate-200/60 transition-colors cursor-pointer shrink-0"
          >
            <X className="w-4 h-4" />
          </button>
        </div>

        <div className="flex-1 overflow-y-auto p-5 text-xs">{children}</div>

        {footer && (
          <div className="px-5 py-3 bg-slate-50/80 border-t border-slate-100 flex items-center justify-end gap-2 shrink-0">
            {footer}
          </div>
        )}
      </div>
    </div>
  )
}
