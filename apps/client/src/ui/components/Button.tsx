/**
 * Button —— 基于微软官方 @fluentui/react-components v9 的 Fluent 2 Button。
 * 保留本项目调用 API，内部转发给官方 Button（不手写任何按钮视觉）。
 */
import type { ReactNode, MouseEventHandler, KeyboardEventHandler } from 'react';
import { Button as FluentButton, Spinner } from '@fluentui/react-components';

export type ButtonVariant = 'primary' | 'secondary' | 'ghost' | 'danger';
export type ButtonSize = 'sm' | 'md' | 'lg';

export interface ButtonProps {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  block?: boolean;
  icon?: ReactNode;
  disabled?: boolean;
  children?: ReactNode;
  className?: string;
  type?: 'button' | 'submit' | 'reset';
  onClick?: MouseEventHandler<HTMLButtonElement>;
  title?: string;
  'aria-label'?: string;
  'aria-current'?: 'page' | undefined;
  name?: string;
  value?: string;
}

const APPEARANCE = {
  primary: 'primary',
  secondary: 'secondary',
  ghost: 'transparent',
  danger: 'primary',
} as const;

const FSIZE = { sm: 'small', md: 'medium', lg: 'large' } as const;

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
  return (
    <FluentButton
      type={type}
      appearance={APPEARANCE[variant]}
      size={FSIZE[size]}
      disabled={disabled || loading}
      aria-busy={loading || undefined}
      icon={loading ? <Spinner size="tiny" /> : (icon as React.ReactElement)}
      className={`${block ? 'w-full' : ''} ${variant === 'danger' ? 'ufot-danger-button' : ''} ${className}`}
      {...rest}
    >
      {children}
    </FluentButton>
  );
}
