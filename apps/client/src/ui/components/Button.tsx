/**
 * Button（Fluent / WinUI 3）：primary / secondary / ghost / danger 四变体 × 三尺寸。
 * 视觉走语义令牌（--accent-* / --bg-surface / --border-input），
 * 状态覆盖：hover / active / focus-visible / disabled / loading（宽度锁定）。
 */
import type { ButtonHTMLAttributes, ReactNode } from 'react';
import { SpinnerIcon } from './icons';
import { useDelayedLoading } from '../hooks/useDelayedFlag';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';
export type ButtonSize = 'sm' | 'md' | 'lg';

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  block?: boolean;
  icon?: ReactNode;
}

const VARIANTS: Record<ButtonVariant, string> = {
  primary:
    'bg-accent text-accent-fg border border-transparent shadow-sm hover:bg-accent-hover active:bg-accent-pressed disabled:bg-primary-100 dark:disabled:bg-primary-50 disabled:text-ink/40',
  secondary:
    'bg-surface text-ink border border-line-input hover:bg-surface-2 hover:border-ink/30 active:bg-surface-2 active:border-ink/40 disabled:text-ink-tertiary disabled:border-line',
  ghost:
    'bg-transparent text-accent border border-transparent hover:bg-accent/[0.06] active:bg-accent/[0.12] disabled:text-ink-tertiary',
  danger:
    'bg-danger-500 text-ink-inverse border border-transparent hover:bg-danger-600 active:bg-danger-700 disabled:bg-danger-100 disabled:text-danger-900/60',
};

const SIZES: Record<ButtonSize, string> = {
  sm: 'h-control-sm px-2 text-body-sm rounded-sm gap-1',
  md: 'h-control-md px-4 text-body rounded-md gap-2',
  lg: 'h-control-lg px-6 text-body rounded-md gap-2',
};

export function Button({
  variant = 'primary',
  size = 'md',
  loading = false,
  block = false,
  icon,
  disabled,
  children,
  className = '',
  type = 'button',
  ...rest
}: ButtonProps) {
  const showSpinner = useDelayedLoading(loading);
  return (
    <button
      type={type}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      className={[
        'fluent-focus inline-flex select-none items-center justify-center font-medium',
        'transition-[background-color,border-color,box-shadow] duration-micro ease-out',
        'disabled:cursor-not-allowed',
        VARIANTS[variant],
        SIZES[size],
        block ? 'w-full' : '',
        className,
      ]
        .filter(Boolean)
        .join(' ')}
      {...rest}
    >
      {icon !== undefined && (
        <span className="inline-flex h-4 w-4 shrink-0 items-center justify-center">
          {loading && showSpinner ? <SpinnerIcon size={size === 'sm' ? 14 : 16} /> : icon}
        </span>
      )}
      <span className="relative inline-flex items-center justify-center">
        {loading && showSpinner && icon === undefined && (
          <SpinnerIcon size={size === 'sm' ? 14 : 16} className="absolute -start-6" />
        )}
        {children}
      </span>
    </button>
  );
}
