import { useEffect, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { cn } from '../../lib/format';
import { IconX } from './icons';
import { t } from '../i18n';

/**
 * 后台模态：radius-xl + shadow-lg + scrim（§1.5 / §10.4）。
 * 键盘：Esc 关闭、Tab 在面板内循环、打开时聚焦第一个可交互元素。
 */
export function Modal({
  open,
  title,
  description,
  tone = 'primary',
  width = 520,
  onClose,
  children,
  footer,
  busy = false,
}: {
  open: boolean;
  title: ReactNode;
  description?: ReactNode;
  tone?: 'primary' | 'danger';
  width?: number;
  onClose: () => void;
  children?: ReactNode;
  footer?: ReactNode;
  busy?: boolean;
}) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!open) return undefined;
    const previous = document.activeElement as HTMLElement | null;
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape' && !busy) {
        event.stopPropagation();
        onClose();
        return;
      }
      if (event.key !== 'Tab' || !panelRef.current) return;
      const focusable = panelRef.current.querySelectorAll<HTMLElement>(
        'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
      );
      if (focusable.length === 0) return;
      const first = focusable[0]!;
      const last = focusable[focusable.length - 1]!;
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', onKeyDown);
    const overflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    window.setTimeout(() => {
      const target = panelRef.current?.querySelector<HTMLElement>(
        'input, textarea, select, button[data-autofocus], button',
      );
      target?.focus();
    }, 0);
    return () => {
      document.removeEventListener('keydown', onKeyDown);
      document.body.style.overflow = overflow;
      previous?.focus?.();
    };
  }, [open, busy, onClose]);

  if (!open) return null;

  return createPortal(
    <div
      className="fixed inset-0 z-modal flex items-center justify-center bg-scrim/40 p-4 dark:bg-scrim/60"
      onMouseDown={(event) => {
        if (event.target === event.currentTarget && !busy) onClose();
      }}
    >
      <div
        ref={panelRef}
        role="dialog"
        aria-modal="true"
        aria-label={typeof title === 'string' ? title : t('common.dialog')}
        className="w-full overflow-hidden rounded-xl bg-bg-elevated shadow-lg"
        style={{ maxWidth: width }}
      >
        <div className="flex items-start gap-3 px-6 pt-5">
          <div className="min-w-0 flex-1">
            <h2 className={cn('text-title', tone === 'danger' ? 'text-danger-600' : 'text-text-primary')}>{title}</h2>
            {description && <div className="mt-1 text-body-sm text-text-secondary">{description}</div>}
          </div>
          <button
            type="button"
            className="btn btn-ghost btn-sm -mr-2"
            onClick={onClose}
            disabled={busy}
            aria-label={t('modal.close')}
          >
            <IconX size={16} />
          </button>
        </div>
        {children && <div className="px-6 py-4">{children}</div>}
        {footer && <div className="flex justify-end gap-2 border-t border-border-default bg-bg-surface-2 px-6 py-3">{footer}</div>}
      </div>
    </div>,
    document.body,
  );
}
