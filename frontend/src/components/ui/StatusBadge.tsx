type Tone = 'emerald' | 'blue' | 'amber' | 'rose' | 'slate'

const STATUS_TONES: Record<string, Tone> = {
  ACTIVE: 'emerald',
  HEALTHY: 'emerald',
  CREDITED: 'emerald',
  COMPLETED: 'emerald',
  CONFIRMED: 'emerald',
  FINALIZED: 'emerald',
  CONFIRMING: 'blue',
  BROADCASTED: 'blue',
  BROADCASTING: 'blue',
  SIGNING: 'blue',
  SIGNED: 'blue',
  CHECKING: 'blue',
  DEGRADED: 'amber',
  LOW_BALANCE: 'amber',
  BROADCAST_UNKNOWN: 'amber',
  PENDING: 'amber',
  DISABLED: 'rose',
  FAILED: 'rose',
  DOWN: 'rose',
  REORGED: 'rose',
  CREATED: 'slate',
  DETECTED: 'slate',
}

const TONE_CLASSES: Record<Tone, { pill: string; dot: string }> = {
  emerald: {
    pill: 'bg-emerald-50 text-emerald-700 border-emerald-200',
    dot: 'bg-emerald-500',
  },
  blue: {
    pill: 'bg-blue-50 text-blue-700 border-blue-200',
    dot: 'bg-blue-500',
  },
  amber: {
    pill: 'bg-amber-50 text-amber-800 border-amber-200',
    dot: 'bg-amber-500',
  },
  rose: {
    pill: 'bg-rose-50 text-rose-700 border-rose-200',
    dot: 'bg-rose-500',
  },
  slate: {
    pill: 'bg-slate-100 text-slate-700 border-slate-200',
    dot: 'bg-slate-400',
  },
}

const PULSE_STATUSES = new Set([
  'CONFIRMING',
  'BROADCASTED',
  'BROADCASTING',
  'SIGNING',
  'SIGNED',
  'CHECKING',
])

export function StatusBadge({
  status,
  showDot = true,
  className = '',
}: {
  status: string
  showDot?: boolean
  className?: string
}) {
  const normalized = (status || 'UNKNOWN').toUpperCase()
  const tone = STATUS_TONES[normalized] ?? 'slate'
  const style = TONE_CLASSES[tone]
  const pulse = PULSE_STATUSES.has(normalized)

  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-semibold font-mono leading-tight tracking-tight whitespace-nowrap ${style.pill} ${className}`}
    >
      {showDot && (
        <span
          className={`w-1.5 h-1.5 rounded-full shrink-0 ${style.dot} ${pulse ? 'animate-pulse' : ''}`}
        />
      )}
      <span>{status}</span>
    </span>
  )
}
