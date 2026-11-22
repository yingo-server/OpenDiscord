/**
 * NetworkBar：离线 / 重连中 / 已恢复，28px 细条，绝不弹窗打断输入。
 * Fluent 语义：InfoBar 的浅色底 + 状态色文字，细分隔线。
 */
import { useEffect } from 'react';
import { useConnStore } from '../../core/stores/conn-store';
import { selectNetworkBanner } from '../../core/selectors';
import { t } from '../i18n';
import { SpinnerIcon, WifiOffIcon } from './icons';

export function NetworkBar({ className = '' }: { className?: string }) {
  const conn = useConnStore();
  const banner = selectNetworkBanner(conn);

  useEffect(() => {
    if (!conn.restoredAt) return;
    const timer = setTimeout(() => conn.dismissRestored(), 1_600);
    return () => clearTimeout(timer);
  }, [conn.restoredAt, conn]);

  if (banner === 'none') return null;

  const tone =
    banner === 'offline'
      ? 'border-b border-line bg-danger-50 text-danger-700'
      : banner === 'restored'
        ? 'border-b border-line bg-success-50 text-success-700'
        : 'border-b border-line bg-warning-50 text-warning-700';

  const label =
    banner === 'offline'
      ? t('network.offline')
      : banner === 'restored'
        ? t('network.restored')
        : t('network.connecting');

  return (
    <div
      role="status"
      aria-live="polite"
      className={`z-network-bar flex h-7 shrink-0 animate-fade-in items-center justify-center gap-2 border-y border-line/60 px-3 text-caption ${tone} ${className}`}
    >
      {banner === 'offline' ? (
        <WifiOffIcon size={14} />
      ) : banner === 'restored' ? (
        <span aria-hidden="true">✓</span>
      ) : (
        <SpinnerIcon size={12} />
      )}
      <span className="truncate">{label}</span>
      {conn.state === 'online' && conn.latencyMs > 0 && banner !== 'offline' ? (
        <span className="nums opacity-60">{t('network.latency', { ms: conn.latencyMs })}</span>
      ) : null}
    </div>
  );
}
