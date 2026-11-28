/** 对话框：桌面居中 Modal，移动端自动变成底部抽屉（拇指可达）。 */

import { useEffect, type ReactNode } from 'react';
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

export function Dialog({ open, title, onClose, children, actions, dismissable = true }: DialogProps) {
  useEffect(() => {
    if (!open) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
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
      <div className="uf-dialog" role="dialog" aria-modal="true" aria-label={title}>
        <div className="uf-row" style={{ justifyContent: 'space-between', marginBottom: 'var(--sp-3)' }}>
          <h2 className="uf-dialog__title" style={{ margin: 0 }}>
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
