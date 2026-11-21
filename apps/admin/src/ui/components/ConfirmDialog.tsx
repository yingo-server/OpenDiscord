import { useEffect, useState, type ReactNode } from 'react';
import { Modal } from './Modal';
import { IconAlert } from './icons';
import { t } from '../i18n';

export interface ConfirmInput {
  totp?: string;
}

/**
 * 破坏性操作二次确认（§9.3）。
 * - danger 一律走这里；广播发布要求输入确认词。
 * - 可选二次 TOTP（security-spec A-3 高危操作）。
 * - 确认中禁关闭，错误就地展示，不弹 toast。
 */
export function ConfirmDialog({
  open,
  title,
  description,
  tone = 'danger',
  confirmLabel = t('confirm.defaultConfirm'),
  cancelLabel = t('confirm.defaultCancel'),
  confirmWord,
  requireTotp = false,
  busy = false,
  error,
  onCancel,
  onConfirm,
  children,
}: {
  open: boolean;
  title: string;
  description?: ReactNode;
  tone?: 'danger' | 'primary';
  confirmLabel?: string;
  cancelLabel?: string;
  confirmWord?: string | null;
  requireTotp?: boolean;
  busy?: boolean;
  error?: string | null;
  onCancel: () => void;
  onConfirm: (input: ConfirmInput) => void;
  children?: ReactNode;
}) {
  const [typed, setTyped] = useState('');
  const [totp, setTotp] = useState('');

  useEffect(() => {
    if (open) {
      setTyped('');
      setTotp('');
    }
  }, [open]);

  const wordOk = !confirmWord || typed.trim() === confirmWord;
  const totpOk = !requireTotp || /^\d{6}$/.test(totp);
  const canConfirm = wordOk && totpOk && !busy;

  return (
    <Modal
      open={open}
      title={title}
      description={description}
      tone={tone}
      onClose={onCancel}
      busy={busy}
      footer={
        <>
          <button type="button" className="btn btn-secondary" onClick={onCancel} disabled={busy}>
            {cancelLabel}
          </button>
          <button
            type="button"
            className={tone === 'danger' ? 'btn btn-danger' : 'btn btn-primary'}
            disabled={!canConfirm}
            onClick={() => onConfirm({ totp: requireTotp ? totp : undefined })}
          >
            {busy ? t('common.executing') : confirmLabel}
          </button>
        </>
      }
    >
      <div className="space-y-4">
        {tone === 'danger' && (
          <div className="flex items-start gap-2 rounded-md bg-danger-50 px-3 py-2 text-body-sm text-danger-700">
            <IconAlert size={16} className="mt-0.5 shrink-0" />
            <span>{t('confirm.dangerHint')}</span>
          </div>
        )}

        {children}

        {confirmWord && (
          <div>
            <label className="label" htmlFor="confirm-word">
              请输入确认词 <span className="code ml-1">{confirmWord}</span>
            </label>
            <input
              id="confirm-word"
              className="input"
              value={typed}
              autoComplete="off"
              spellCheck={false}
              onChange={(event) => setTyped(event.target.value)}
              placeholder={confirmWord}
            />
          </div>
        )}

        {requireTotp && (
          <div>
            <label className="label" htmlFor="confirm-totp">
              动态验证码（6 位）
            </label>
            <input
              id="confirm-totp"
              className="input nums tracking-[0.4em]"
              inputMode="numeric"
              maxLength={6}
              value={totp}
              autoComplete="one-time-code"
              onChange={(event) => setTotp(event.target.value.replace(/\D/g, '').slice(0, 6))}
              placeholder="000000"
            />
          </div>
        )}

        {error && <p className="text-body-sm text-danger-600">{error}</p>}
      </div>
    </Modal>
  );
}
