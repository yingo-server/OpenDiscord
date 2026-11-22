/**
 * Toast（Fluent / WinUI 3 InfoBar 风格）：单例队列、最多同时 1 条、3s（含操作 5s）
 * 自动消失。位置：移动端底部安全区上方，桌面右上角；不覆盖输入区。
 * 使用 Acrylic 材质 + 语义色条 + 8px 圆角与 Fluent 阴影。
 */
import { useEffect, useState, type ReactNode } from 'react';

export type ToastKind = 'info' | 'success' | 'danger';

export interface ToastOptions {
  kind?: ToastKind;
  durationMs?: number;
  actionLabel?: string;
  onAction?: () => void;
}

interface ToastItem {
  id: number;
  message: ReactNode;
  kind: ToastKind;
  actionLabel?: string;
  onAction?: () => void;
}

let current: ToastItem | null = null;
let timer: ReturnType<typeof setTimeout> | null = null;
let nextId = 1;
const listeners = new Set<(item: ToastItem | null) => void>();

function publish(): void {
  for (const listener of listeners) listener(current);
}

function dismiss(): void {
  if (timer) {
    clearTimeout(timer);
    timer = null;
  }
  current = null;
  publish();
}

function show(message: ReactNode, options: ToastOptions = {}): void {
  if (timer) clearTimeout(timer);
  current = {
    id: nextId++,
    message,
    kind: options.kind ?? 'info',
    actionLabel: options.actionLabel,
    onAction: options.onAction,
  };
  publish();
  const duration = options.durationMs ?? (options.actionLabel ? 5_000 : 3_000);
  timer = setTimeout(dismiss, duration);
}

export const toast = {
  show,
  info: (message: ReactNode, options: Omit<ToastOptions, 'kind'> = {}) =>
    show(message, { ...options, kind: 'info' }),
  success: (message: ReactNode, options: Omit<ToastOptions, 'kind'> = {}) =>
    show(message, { ...options, kind: 'success' }),
  danger: (message: ReactNode, options: Omit<ToastOptions, 'kind'> = {}) =>
    show(message, { ...options, kind: 'danger' }),
  dismiss,
};

const STRIPE: Record<ToastKind, string> = {
  info: 'bg-accent',
  success: 'bg-success-500',
  danger: 'bg-danger-500',
};

const ICON: Record<ToastKind, string> = {
  info: 'text-accent',
  success: 'text-success-500',
  danger: 'text-danger-500',
};

export function Toaster() {
  const [item, setItem] = useState<ToastItem | null>(current);

  useEffect(() => {
    listeners.add(setItem);
    return () => {
      listeners.delete(setItem);
    };
  }, []);

  if (!item) return null;

  return (
    <div
      className="pointer-events-none fixed inset-x-4 bottom-4 z-toast flex justify-center desktop:inset-x-auto desktop:bottom-auto desktop:end-6 desktop:top-6"
      role="status"
      aria-live="polite"
    >
      <div
        key={item.id}
        className="acrylic pointer-events-auto flex max-w-[calc(100vw-32px)] animate-slide-up items-center gap-3 overflow-hidden rounded-lg py-2.5 pe-4 ps-3.5 text-body-sm text-ink shadow-md"
      >
        <span className={`absolute inset-y-0 start-0 w-1 ${STRIPE[item.kind]}`} aria-hidden="true" />
        <span className={`ms-1 inline-block h-2 w-2 shrink-0 rounded-full ${ICON[item.kind]}`} aria-hidden="true" />
        <span className="min-w-0 break-words">{item.message}</span>
        {item.actionLabel && (
          <button
            type="button"
            className="fluent-focus shrink-0 rounded-sm px-1 font-medium text-accent underline underline-offset-2"
            onClick={() => {
              item.onAction?.();
              dismiss();
            }}
          >
            {item.actionLabel}
          </button>
        )}
      </div>
    </div>
  );
}
