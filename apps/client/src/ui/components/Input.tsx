/**
 * Input —— 基于微软官方 @fluentui/react-components v9 的 Input / Label / Text。
 * 保留本项目 API（label/error/hint/icon/onClear/onChange(value)），内部全用官方组件：
 * 错误态用官方 Input 的 validationState="error" + 官方 Text 错误文案。
 */
import { useId, type ReactNode } from 'react';
import {
  Input as FluentInput,
  Label,
  Text,
} from '@fluentui/react-components';

export interface InputProps {
  label?: string;
  error?: string;
  hint?: ReactNode;
  icon?: ReactNode;
  onClear?: () => void;
  onChange?: (value: string) => void;
  id?: string;
  className?: string;
  value?: string;
  placeholder?: string;
  type?: 'text' | 'password' | 'email' | 'tel' | 'number' | 'search' | 'url';
  autoComplete?: string;
  inputMode?: 'text' | 'numeric' | 'tel' | 'email' | 'url';
  maxLength?: number;
  disabled?: boolean;
  onKeyDown?: (event: React.KeyboardEvent<HTMLInputElement>) => void;
  onFocus?: (event: React.FocusEvent<HTMLInputElement>) => void;
  onBlur?: (event: React.FocusEvent<HTMLInputElement>) => void;
  'aria-label'?: string;
  name?: string;
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
  return (
    <div className={`w-full ${className}`}>
      {label && <Label htmlFor={inputId}>{label}</Label>}
      <FluentInput
        id={inputId}
        value={value}
        onChange={(_, data) => onChange?.(data.value)}
        contentBefore={icon ? (icon as React.ReactElement) : undefined}
        contentAfter={onClear && value ? <button type="button" onClick={onClear} aria-label="clear">✕</button> : undefined}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? errorId : undefined}
        className={error ? 'ufot-input-error' : undefined}
        {...rest}
      />
      {error ? (
        <Text id={errorId} size={200} className="ufot-error-text">{error}</Text>
      ) : hint ? (
        <Text size={200}>{hint}</Text>
      ) : null}
    </div>
  );
}
