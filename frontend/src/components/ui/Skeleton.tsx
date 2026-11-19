export function Skeleton({ className = '' }: { className?: string }) {
  return <div className={`animate-pulse rounded bg-slate-200/75 ${className}`} />
}

export function TableSkeleton({ rows = 5, cols = 5 }: { rows?: number; cols?: number }) {
  return (
    <div className="p-4 space-y-3" aria-busy="true" aria-label="加载中">
      <div className="grid gap-3" style={{ gridTemplateColumns: `repeat(${cols}, minmax(0, 1fr))` }}>
        {Array.from({ length: cols }).map((_, i) => (
          <Skeleton key={i} className="h-3.5 w-20" />
        ))}
      </div>
      <div className="divide-y divide-slate-100">
        {Array.from({ length: rows }).map((_, rowIdx) => (
          <div
            key={rowIdx}
            className="py-3 grid gap-3 items-center"
            style={{ gridTemplateColumns: `repeat(${cols}, minmax(0, 1fr))` }}
          >
            {Array.from({ length: cols }).map((_, colIdx) => (
              <Skeleton key={colIdx} className={`h-4 ${colIdx === 0 ? 'w-24' : 'w-full'}`} />
            ))}
          </div>
        ))}
      </div>
    </div>
  )
}

export function CardSkeleton({ lines = 3 }: { lines?: number }) {
  return (
    <div className="p-4 space-y-2.5" aria-busy="true">
      {Array.from({ length: lines }).map((_, idx) => (
        <Skeleton key={idx} className={`h-4 ${idx === 0 ? 'w-1/3' : idx === lines - 1 ? 'w-2/3' : 'w-full'}`} />
      ))}
    </div>
  )
}
