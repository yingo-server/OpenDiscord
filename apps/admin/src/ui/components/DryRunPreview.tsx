import type { DryRunResult } from '../../api/types';
import { formatNumber } from '../../lib/format';
import { IconAlert } from './icons';
import { t } from '../i18n';

const LABELS: Record<string, string> = {
  users: t('dryRun.users'),
  rows: t('dryRun.rows'),
  messages: t('dryRun.messages'),
  conversations: t('dryRun.conversations'),
  recipients: t('dryRun.recipients'),
  skipped: t('dryRun.skipped'),
  duration_sec: t('dryRun.duration'),
  bytes: t('dryRun.bytes'),
  audit_rows: t('dryRun.auditRows'),
};

function humanize(key: string): string {
  return LABELS[key] ?? key.replace(/_/g, ' ');
}

function renderValue(key: string, value: number | string): string {
  if (typeof value === 'number') {
    if (key.endsWith('_bytes') || key === 'bytes') {
      const units = ['B', 'KB', 'MB', 'GB'];
      let n = value;
      let i = 0;
      while (n >= 1024 && i < units.length - 1) {
        n /= 1024;
        i += 1;
      }
      return `${n.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
    }
    return formatNumber(value);
  }
  return value;
}

/**
 * dry-run 预览（docs/admin-data-design.md §8：?dry_run=true 返回预估，不产审计动作行）。
 * 放在 ConfirmDialog 内部 children 位置，与确认按钮同屏 —— 管理者看到"影响面"再点确认。
 */
export function DryRunPreview({
  result,
  loading = false,
  error = null,
  onRetry,
  compact = false,
}: {
  result: DryRunResult | null;
  loading?: boolean;
  error?: string | null;
  onRetry?: () => void;
  compact?: boolean;
}) {
  if (loading) {
    return (
      <div className="rounded-md border border-border-default bg-bg-surface-2 px-3 py-3">
        <div className="skeleton h-3 w-32" />
        <div className="mt-2 skeleton h-3 w-48" />
      </div>
    );
  }

  if (error) {
    return (
      <div className="flex items-start gap-2 rounded-md border border-danger-300 bg-danger-50 px-3 py-2 text-body-sm text-danger-700">
        <IconAlert size={16} className="mt-0.5 shrink-0" />
        <div className="min-w-0">
          <p>预估失败：{error}</p>
          {onRetry && (
            <button type="button" className="mt-1 underline" onClick={onRetry}>
              重试
            </button>
          )}
        </div>
      </div>
    );
  }

  if (!result) return null;

  const entries = Object.entries(result.affected ?? {});

  return (
    <div className="rounded-md border border-primary-300 bg-primary-50 px-3 py-3">
      <div className="flex items-center gap-2">
        <span className="rounded-sm bg-primary-500 px-1.5 py-0.5 text-caption font-semibold text-text-inverse">dry-run</span>
        <span className="text-body-sm text-text-secondary">{t('dryRun.hint')}</span>
      </div>
      <p className="mt-2 text-table text-text-primary">{result.summary}</p>
      {entries.length > 0 && (
        <dl className={compact ? 'mt-2 flex flex-wrap gap-x-5 gap-y-1' : 'mt-3 grid grid-cols-2 gap-x-4 gap-y-2'}>
          {entries.map(([key, value]) => (
            <div key={key} className="flex items-baseline gap-2">
              <dt className="text-body-sm text-text-tertiary">{humanize(key)}</dt>
              <dd className="nums text-table font-semibold text-text-primary">{renderValue(key, value)}</dd>
            </div>
          ))}
        </dl>
      )}
      {result.warnings && result.warnings.length > 0 && (
        <ul className="mt-3 space-y-1 border-t border-primary-300/60 pt-2">
          {result.warnings.map((w) => (
            <li key={w} className="flex items-start gap-1.5 text-body-sm text-warning-700">
              <IconAlert size={14} className="mt-0.5 shrink-0" />
              <span>{w}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * 不支持 dry-run 的操作（如撤回单条消息）也要给影响面：
 * 用同一视觉组件呈现静态说明，避免"确认框里只有一句危险提示"。
 */
export function ImpactList({ items }: { items: Array<{ label: string; value: string }> }) {
  return (
    <dl className="grid grid-cols-2 gap-x-4 gap-y-2 rounded-md bg-bg-surface-2 px-3 py-2">
      {items.map((item) => (
        <div key={item.label} className="flex items-baseline gap-2">
          <dt className="text-body-sm text-text-tertiary">{item.label}</dt>
          <dd className="text-table text-text-primary">{item.value}</dd>
        </div>
      ))}
    </dl>
  );
}
