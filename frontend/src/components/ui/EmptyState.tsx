import { Inbox } from 'lucide-react'
import type { ReactNode } from 'react'

type EmptyStateProps = {
  icon?: ReactNode
  title?: string
  description?: string
  action?: ReactNode
  className?: string
}

export function EmptyState({
  icon,
  title,
  description = '暂无数据',
  action,
  className = '',
}: EmptyStateProps) {
  return (
    <div className={`flex flex-col items-center justify-center py-10 px-4 text-center ${className}`}>
      <div className="w-10 h-10 rounded-full bg-slate-100 text-slate-400 flex items-center justify-center mb-2.5">
        {icon ?? <Inbox className="w-5 h-5" />}
      </div>
      {title && <div className="text-xs font-semibold text-slate-700 mb-1">{title}</div>}
      <div className="text-xs text-slate-400 max-w-xs">{description}</div>
      {action && <div className="mt-3">{action}</div>}
    </div>
  )
}
