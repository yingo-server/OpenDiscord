/** 轻提示（toast）：错误用红、警告用黄、提示用蓝。 */

import { useEffect } from 'react';
import { createPortal } from 'react-dom';
import { Icon } from './Icon';
import type { IconName } from './Icon';

export type ToastTone = 'info' | 'error' | 'warn';

export interface ToastItem {
  id: number;
  text: string;
  tone: ToastTone;
}

const ICON_FOR: Record<ToastTone, IconName> = {
  info: 'info',
  error: 'warning',
  warn: 'warning',
};

export function ToastStack({ items }: { items: ToastItem[] }) {
  if (typeof document === 'undefined') return null;
  return createPortal(
    <div className="uf-toasts" role="status" aria-live="polite">
      {items.map((item) => (
        <div key={item.id} className={`uf-toast uf-toast--${item.tone}`}>
          <Icon name={ICON_FOR[item.tone]} size="sm" />
          <span className="uf-grow break-anywhere">{item.text}</span>
        </div>
      ))}
    </div>,
    document.body,
  );
}

export function useToastAutoDismiss(
  items: ToastItem[],
  dismiss: (id: number) => void,
  ttl = 4200,
): void {
  useEffect(() => {
    if (!items.length) return;
    const timer = setTimeout(() => dismiss(items[0]?.id ?? 0), ttl);
    return () => clearTimeout(timer);
  }, [items, dismiss, ttl]);
}
