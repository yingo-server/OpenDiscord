/** 基础控件：按钮、图标按钮、表单字段、标签页、徽标、头像、加载、空态。 */

import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode, TextareaHTMLAttributes } from 'react';
import { useId } from 'react';
import { Icon } from './Icon';
import type { IconName } from './Icon';

type ButtonVariant = 'default' | 'primary' | 'danger' | 'ghost';

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  block?: boolean;
  small?: boolean;
  icon?: IconName;
  iconRight?: IconName;
  loading?: boolean;
}

export function Button({
  variant = 'default',
  block,
  small,
  icon,
  iconRight,
  loading,
  children,
  className,
  disabled,
  ...rest
}: ButtonProps) {
  const classes = [
    'uf-btn',
    variant !== 'default' ? `uf-btn--${variant}` : '',
    block ? 'uf-btn--block' : '',
    small ? 'uf-btn--sm' : '',
    className ?? '',
  ]
    .filter(Boolean)
    .join(' ');
  return (
    <button className={classes} disabled={disabled || loading} {...rest}>
      {loading ? <Icon name="spinner" spin /> : icon ? <Icon name={icon} /> : null}
      {children}
      {iconRight ? <Icon name={iconRight} /> : null}
    </button>
  );
}

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  icon: IconName;
  label: string;
  size?: 'sm' | 'md' | 'lg';
  active?: boolean;
  danger?: boolean;
}

export function IconButton({
  icon,
  label,
  size = 'md',
  active,
  danger,
  className,
  ...rest
}: IconButtonProps) {
  const classes = [
    'uf-iconbtn',
    active ? 'uf-iconbtn--active' : '',
    danger ? 'uf-iconbtn--danger' : '',
    className ?? '',
  ]
    .filter(Boolean)
    .join(' ');
  return (
    <button className={classes} aria-label={label} title={label} {...rest}>
      <Icon name={icon} size={size} />
    </button>
  );
}

export interface FieldProps {
  label?: string;
  hint?: string;
  error?: string | null;
  children: (id: string) => ReactNode;
}

export function Field({ label, hint, error, children }: FieldProps) {
  const id = useId();
  const errorId = `${id}-error`;
  const hintId = `${id}-hint`;
  return (
    <div className="uf-field">
      {label ? (
        <label className="uf-label" htmlFor={id}>
          {label}
        </label>
      ) : null}
      {children(id)}
      {error ? (
        <span id={errorId} className="uf-error" role="alert">
          {error}
        </span>
      ) : hint ? (
        <span id={hintId} className="uf-hint">
          {hint}
        </span>
      ) : null}
    </div>
  );
}

export interface TextInputProps extends InputHTMLAttributes<HTMLInputElement> {
  invalid?: boolean;
}

export function TextInput({ invalid, className, ...rest }: TextInputProps) {
  return (
    <input
      className={['uf-input', invalid ? 'uf-input--invalid' : '', className ?? ''].filter(Boolean).join(' ')}
      {...rest}
    />
  );
}

export interface TextAreaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  invalid?: boolean;
}

export function TextArea({ invalid, className, ...rest }: TextAreaProps) {
  return (
    <textarea
      className={['uf-textarea', invalid ? 'uf-input--invalid' : '', className ?? ''].filter(Boolean).join(' ')}
      {...rest}
    />
  );
}

export interface TabsProps<T extends string> {
  value: T;
  options: { value: T; label: string; disabled?: boolean }[];
  onChange: (value: T) => void;
  ariaLabel?: string;
}

export function Tabs<T extends string>({ value, options, onChange, ariaLabel }: TabsProps<T>) {
  const handleKeyDown = (event: React.KeyboardEvent, index: number) => {
    if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
    event.preventDefault();
    const delta = event.key === 'ArrowRight' ? 1 : -1;
    const next = (index + delta + options.length) % options.length;
    const option = options[next];
    if (option && !option.disabled) onChange(option.value);
  };

  return (
    <div className="uf-tabs" role="radiogroup" aria-label={ariaLabel}>
      {options.map((option, index) => (
        <button
          key={option.value}
          role="radio"
          type="button"
          className="uf-tab"
          aria-checked={option.value === value}
          tabIndex={option.value === value ? 0 : -1}
          disabled={option.disabled}
          onClick={() => onChange(option.value)}
          onKeyDown={(event) => handleKeyDown(event, index)}
        >
          {option.label}
        </button>
      ))}
    </div>
  );
}

export function Badge({
  count,
  tone = 'warn',
}: {
  count: number;
  tone?: 'warn' | 'blue' | 'red';
}) {
  if (!count) return null;
  const toneClass = tone === 'blue' ? 'uf-badge--blue' : tone === 'red' ? 'uf-badge--red' : '';
  return <span className={`uf-badge ${toneClass}`}>{count > 99 ? '99+' : count}</span>;
}

export function Avatar({
  name,
  size = 'md',
  online,
}: {
  name?: string;
  size?: 'sm' | 'md' | 'lg';
  online?: boolean;
}) {
  const initial = (name ?? '?').trim().slice(0, 1).toUpperCase();
  const sizeClass = size === 'lg' ? 'uf-avatar--lg' : size === 'sm' ? 'uf-avatar--sm' : '';
  return (
    <span
      className={`uf-avatar ${sizeClass} ${online ? 'uf-avatar--online' : ''}`}
      title={name}
      aria-hidden="true"
    >
      {initial}
    </span>
  );
}

export function Spinner() {
  return <Icon name="spinner" spin className="uf-spinner" />;
}

export function EmptyState({
  icon = 'chat',
  title,
  description,
  action,
}: {
  icon?: IconName;
  title: string;
  description?: string;
  action?: ReactNode;
}) {
  return (
    <div className="uf-empty">
      <Icon name={icon} size="lg" className="uf-empty__icon" />
      <div className="uf-col" style={{ gap: 'var(--sp-1)' }}>
        <strong>{title}</strong>
        {description ? <span className="uf-faint">{description}</span> : null}
      </div>
      {action}
    </div>
  );
}
