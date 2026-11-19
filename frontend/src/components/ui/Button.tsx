import { LoaderCircle } from 'lucide-react'
import type { AnchorHTMLAttributes, ButtonHTMLAttributes, ReactNode } from 'react'

export type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost'
export type ButtonSize = 'xs' | 'sm' | 'md' | 'lg'

type BaseProps = {
  variant?: ButtonVariant
  size?: ButtonSize
  loading?: boolean
  icon?: ReactNode
  block?: boolean
  className?: string
  children?: ReactNode
}

export type ButtonProps = BaseProps &
  ButtonHTMLAttributes<HTMLButtonElement> & {
    href?: undefined
  }

export type AnchorButtonProps = BaseProps &
  AnchorHTMLAttributes<HTMLAnchorElement> & {
    href: string
    disabled?: boolean
  }

const variantClasses: Record<ButtonVariant, string> = {
  primary:
    'bg-[#09182b] hover:bg-[#122640] active:bg-[#06101e] text-white border border-transparent shadow-xs disabled:bg-slate-200 disabled:text-slate-400',
  secondary:
    'bg-white hover:bg-slate-50 active:bg-slate-100 text-slate-700 border border-slate-200 shadow-xs disabled:bg-slate-50 disabled:text-slate-400',
  danger:
    'bg-rose-600 hover:bg-rose-700 active:bg-rose-800 text-white border border-transparent shadow-xs disabled:bg-rose-200 disabled:text-white',
  ghost:
    'bg-transparent hover:bg-slate-100 active:bg-slate-200/70 text-slate-600 hover:text-slate-900 border border-transparent disabled:text-slate-300',
}

const sizeClasses: Record<ButtonSize, string> = {
  xs: 'h-7 px-2 text-[11px] gap-1 rounded-md',
  sm: 'h-8 px-2.5 text-xs gap-1.5 rounded-md',
  md: 'h-9 px-3.5 text-xs gap-1.5 rounded-md',
  lg: 'h-10 px-4 text-sm gap-2 rounded-lg',
}

export function Button(props: ButtonProps | AnchorButtonProps) {
  const {
    variant = 'secondary',
    size = 'md',
    loading = false,
    icon,
    block = false,
    className = '',
    children,
    ...rest
  } = props

  const classes = [
    'inline-flex items-center justify-center font-medium transition-colors whitespace-nowrap select-none cursor-pointer disabled:cursor-not-allowed',
    variantClasses[variant],
    sizeClasses[size],
    block ? 'w-full' : '',
    className,
  ]
    .filter(Boolean)
    .join(' ')

  const content = (
    <>
      {loading ? <LoaderCircle className="w-3.5 h-3.5 animate-spin shrink-0" /> : icon ? <span className="shrink-0 inline-flex">{icon}</span> : null}
      {children}
    </>
  )

  if ('href' in props && props.href !== undefined) {
    const { href, disabled, ...anchorRest } = rest as AnchorHTMLAttributes<HTMLAnchorElement> & { disabled?: boolean }
    return (
      <a
        href={disabled ? undefined : href}
        aria-disabled={disabled}
        className={classes}
        {...anchorRest}
      >
        {content}
      </a>
    )
  }

  const { disabled, type = 'button', ...buttonRest } = rest as ButtonHTMLAttributes<HTMLButtonElement>
  return (
    <button
      type={type}
      disabled={disabled || loading}
      className={classes}
      {...buttonRest}
    >
      {content}
    </button>
  )
}
