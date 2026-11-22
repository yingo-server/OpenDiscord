/**
 * IconButton（Fluent）：统一图标按钮外壳，收敛 40/44px 点击区、hover / active /
 * focus-visible / disabled 状态，避免每个视图重复手写圆形按钮。
 * 业务按钮只需传 aria-label、图标与 onClick。
 */
import type { ButtonHTMLAttributes, ReactNode } from 'react';

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  label: string;
  icon: ReactNode;
  size?: 'sm' | 'md' | 'lg';
}

const SIZE: Record<NonNullable<IconButtonProps['size']>, string> = {
  sm: 'h-9 w-9',
  md: 'h-10 w-10',
  lg: 'h-11 w-11',
};

export function IconButton({
  label,
  icon,
  size = 'md',
  className = '',
  type = 'button',
  ...rest
}: IconButtonProps) {
  return (
    <button
      type={type}
      aria-label={label}
      title={label}
      className={[
        'inline-flex shrink-0 items-center justify-center rounded-md text-ink-secondary',
        'transition-colors duration-micro ease-out',
        'hover:bg-ink/[0.06] hover:text-ink active:bg-ink/[0.1]',
        'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus',
        'disabled:cursor-not-allowed disabled:opacity-40',
        SIZE[size],
        className,
      ]
        .filter(Boolean)
        .join(' ')}
      {...rest}
    >
      {icon}
    </button>
  );
}
