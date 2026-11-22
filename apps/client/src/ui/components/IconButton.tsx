/**
 * IconButton —— 基于微软官方 @fluentui/react-components v9 的 icon-only Button。
 */
import type { ReactElement, MouseEventHandler } from 'react';
import { Button as FluentButton } from '@fluentui/react-components';

export interface IconButtonProps {
  label: string;
  icon: ReactElement;
  size?: 'sm' | 'md' | 'lg';
  onClick?: MouseEventHandler<HTMLButtonElement>;
  className?: string;
  disabled?: boolean;
  title?: string;
  'aria-current'?: 'page' | undefined;
}

const FSIZE = { sm: 'small', md: 'medium', lg: 'large' } as const;

export function IconButton({
  label,
  icon,
  size = 'md',
  className = '',
  ...rest
}: IconButtonProps) {
  return (
    <FluentButton
      appearance="subtle"
      size={FSIZE[size]}
      icon={icon}
      aria-label={label}
      title={label}
      className={className}
      {...rest}
    />
  );
}
