/** 对话框：MD1 风格；打开时聚焦、Tab 不出框、关闭后归还焦点。 */

import { useEffect, useId, useRef, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { IconButton } from './controls';

export interface DialogProps {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
  actions?: ReactNode;
  /** 点击遮罩是否关闭（默认 true） */
  dismissable?: boolean;
}

const FOCUSABLE =
  'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

export function Dialog({ open, title, onClose, children, actions, dismissable = true }: DialogProps) {
  const titleId = useId();
  const dialogRef = useRef<HTMLDivElement | null>(null);
  const previousFocus = useRef<HTMLElement | null>(null);

  useEffect(() => {
    if (!open) return;
    previousFocus.current = document.activeElement as HTMLElement | null;
    const dialog = dialogRef.current;
    const first = dialog?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? dialog)?.focus();

    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        onClose();
        return;
      }
      if (event.key !== 'Tab' || !dialog) return;
      const nodes = [...dialog.querySelectorAll<HTMLElement>(FOCUSABLE)];
      if (!nodes.length) return;
      const firstNode = nodes[0]!;
      const lastNode = nodes[nodes.length - 1]!;
      if (event.shiftKey && document.activeElement === firstNode) {
        event.preventDefault();
        lastNode.focus();
      } else if (!event.shiftKey && document.activeElement === lastNode) {
        event.preventDefault();
        firstNode.focus();
      }
    };
    document.addEventListener('keydown', onKey);
    return () => {
      document.removeEventListener('keydown', onKey);
      previousFocus.current?.focus();
    };
  }, [open, onClose]);

  if (!open) return null;

  return createPortal(
    <div
      className="uf-overlay"
      role="presentation"
      onMouseDown={(event) => {
        if (dismissable && event.target === event.currentTarget) onClose();
      }}
    >
      <div
        ref={dialogRef}
        className="uf-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
      >
        <div className="uf-row" style={{ justifyContent: 'space-between', marginBottom: 'var(--sp-3)' }}>
          <h2 id={titleId} className="uf-dialog__title" style={{ margin: 0 }}>
            {title}
          </h2>
          <IconButton icon="close" label="关闭" onClick={onClose} />
        </div>
        {children}
        {actions ? <div className="uf-dialog__actions">{actions}</div> : null}
      </div>
    </div>,
    document.body,
  );
}
