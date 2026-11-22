/**
 * Input（Fluent / WinUI 3）：label / error / hint / 前置图标 / 清除按钮。
 * Fluent TextBox：4px 圆角 + 强边框（--border-input，满足 3:1）+ focus 2px 强调环；
 * 错误态边框 + 文案双重提示（aria-invalid / aria-describedby）。
 */
import { useId, type InputHTMLAttributes, type ReactNode } from 'react';
import { CloseIcon } from './icons';

export interface InputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'onChange'> {
  label?: string;
  error?: string;
  hint?: ReactNode;
  icon?: ReactNode;
  onClear?: () => void;
  onChange?: (value: string) => void;
}

export function Input({
  label,
  error,
  hint,
  icon,
  onClear,
  onChange,
  id,
  className = '',
  value,
  ...rest
}: InputProps) {
  const autoId = useId();
  const inputId = id ?? autoId;
  const errorId = `${inputId}-error`;
  const describedBy = error ? errorId : undefined;

  return (
    <div className="w-full">
      {label && (
        <label htmlFor={inputId} className="mb-1.5 block text-body-sm text-ink-secondary">
          {label}
        </label>
      )}
      <div className="relative">
        {icon && (
          <span className="pointer-events-none absolute inset-y-0 start-3 flex items-center text-ink-tertiary">
            {icon}
          </span>
        )}
        <input
          id={inputId}
          value={value}
          onChange={(event) => onChange?.(event.target.value)}
          aria-invalid={error ? true : undefined}
          aria-describedby={describedBy}
          className={[
            'fluent-focus h-control-md w-full rounded-sm border bg-surface text-body text-ink',
            'transition-[border-color,box-shadow] duration-micro placeholder:text-ink-tertiary',
            icon ? 'ps-9 pe-3' : 'px-3',
            onClear && value ? 'pe-10' : '',
            error
              ? 'border-danger-500 focus:border-danger-500'
              : 'border-line-input hover:border-ink/40 focus:border-accent',
            className,
          ]
            .filter(Boolean)
            .join(' ')}
          {...rest}
        />
        {onClear && value ? (
          <button
            type="button"
            onClick={onClear}
            aria-label="clear"
            className="fluent-focus absolute inset-y-0 end-2 my-auto flex h-6 w-6 items-center justify-center rounded-sm text-ink-tertiary hover:bg-ink/[0.06] hover:text-ink"
          >
            <CloseIcon size={14} />
          </button>
        ) : null}
      </div>
      {error ? (
        <p id={errorId} className="mt-1 text-caption text-danger-500">
          {error}
        </p>
      ) : hint ? (
        <p className="mt-1 text-caption text-ink-tertiary">{hint}</p>
      ) : null}
    </div>
  );
}
