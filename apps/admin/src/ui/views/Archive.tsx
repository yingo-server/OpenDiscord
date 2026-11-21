import { useCallback, useEffect, useState } from 'react';
import { archiveHistory, archiveStatus, runArchive, type ArchiveRun, type ArchiveStatus } from '../../api/archive';

/** ArchiveRun 也有 dry_run 布尔字段，`in` 无法判别，用结构判别 dry-run 结果 */
function isDryRun(res: ArchiveRun | DryRunResult): res is DryRunResult {
  return 'summary' in res && 'affected' in res;
}
import type { DryRunResult } from '../../api/types';
import { errorMessage, useTransientFlag } from '../../lib/hooks';
import { cn, formatBytes, formatDateTime, formatNumber, formatRelative } from '../../lib/format';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { DataTable, type Column } from '../components/DataTable';
import { DryRunPreview } from '../components/DryRunPreview';
import { PageHeader } from '../components/PageHeader';
import { StatusTag } from '../components/StatusTag';
import { IconAlert, IconArchive, IconRefresh } from '../components/icons';
import { t } from '../i18n';

export function Archive() {
  const [status, setStatus] = useState<ArchiveStatus | null>(null);
  const [runs, setRuns] = useState<ArchiveRun[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [reloadKey, setReloadKey] = useState(0);
  const [flash, fireFlash] = useTransientFlag();

  const [preview, setPreview] = useState<DryRunResult | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [runOpen, setRunOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    Promise.all([archiveStatus(controller.signal), archiveHistory(20, controller.signal)])
      .then(([st, history]) => {
        setStatus(st);
        setRuns(history);
        setError(null);
      })
      .catch((err) => {
        if ((err as Error).name === 'AbortError') return;
        setError(errorMessage(err));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [reloadKey]);

  /* 运行中每 5s 刷新一次，结束即停 */
  useEffect(() => {
    if (!status?.running) return undefined;
    const timer = window.setInterval(() => setReloadKey((k) => k + 1), 5000);
    return () => window.clearInterval(timer);
  }, [status?.running]);

  const dryRun = useCallback(async () => {
    setPreviewLoading(true);
    setPreviewError(null);
    setPreview(null);
    try {
      const res = await runArchive({}, { dryRun: true });
      setPreview(isDryRun(res) ? res : null);
      setRunOpen(true);
    } catch (err) {
      setPreviewError(errorMessage(err));
      setRunOpen(true);
    } finally {
      setPreviewLoading(false);
    }
  }, []);

  const running = status?.running ?? false;

  const columns: Column<ArchiveRun>[] = [
    {
      key: 'id',
      header: t('archive.status.running'),
      width: 90,
      render: (r) => <span className="nums text-body-sm text-text-secondary">#{r.id}</span>,
    },
    {
      key: 'status',
      header: t('common.status'),
      width: 110,
      render: (r) => (
        <StatusTag tone={r.status === 'ok' ? 'success' : r.status === 'running' ? 'warning' : 'danger'}>
          {r.status === 'ok' ? t('archive.status.ok') : r.status === 'running' ? t('archive.status.running') : r.status === 'partial' ? t('archive.status.partial') : t('archive.status.failed')}
        </StatusTag>
      ),
    },
    {
      key: 'moved',
      header: t('archive.rows'),
      width: 120,
      align: 'right',
      render: (r) => <span className="nums text-table text-text-primary">{formatNumber(r.moved_rows)}</span>,
    },
    {
      key: 'convs',
      header: t('dryRun.conversations'),
      width: 110,
      align: 'right',
      render: (r) => <span className="nums text-table text-text-secondary">{formatNumber(r.convs_touched)}</span>,
    },
    {
      key: 'started',
      header: t('archive.startedAt'),
      width: 180,
      render: (r) => <span className="nums text-body-sm text-text-secondary">{formatDateTime(r.started_at)}</span>,
    },
    {
      key: 'duration',
      header: t('common.duration'),
      width: 120,
      render: (r) => {
        if (!r.finished_at) return <span className="text-body-sm text-warning-700">{t('archive.inProgress')}</span>;
        const sec = (new Date(r.finished_at).getTime() - new Date(r.started_at).getTime()) / 1000;
        return <span className="nums text-body-sm text-text-secondary">{sec < 60 ? `${Math.round(sec)}s` : `${Math.round(sec / 60)}m`}</span>;
      },
    },
    {
      key: 'error',
      header: t('common.note'),
      render: (r) => <span className="text-body-sm text-text-tertiary">{r.error ?? '—'}</span>,
    },
  ];

  return (
    <>
      <PageHeader
        title={t('archive.title')}
        description={t('archive.description')}
        meta={
          <>
            {status && (
              <span>
                {t('archive.nextRun')} <span className="nums">{formatDateTime(status.next_run_at)}</span>（{formatRelative(status.next_run_at)}）
              </span>
            )}
            {flash && <span className="text-success-600">{t('common.updated')}</span>}
          </>
        }
        actions={
          <>
            <button type="button" className="btn btn-secondary btn-sm" onClick={() => setReloadKey((k) => k + 1)}>
              <IconRefresh size={14} /> 刷新
            </button>
            <button type="button" className="btn btn-secondary btn-sm" onClick={() => void dryRun()} disabled={previewLoading || running}>
              <IconArchive size={14} /> dry-run 预览
            </button>
            <button
              type="button"
              className="btn btn-primary btn-sm"
              disabled={running || loading}
              onClick={() => {
                setActionError(null);
                setRunOpen(true);
                if (!preview) void dryRun();
              }}
              title={running ? t('archive.triggerRunning') : undefined}
            >
              手动触发归档
            </button>
          </>
        }
      />

      {error && (
        <div className="mb-4 flex items-center gap-2 rounded-md bg-danger-50 px-3 py-2 text-body-sm text-danger-700">
          <IconAlert size={16} /> {error}
        </div>
      )}

      <div className="mb-4 grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard
          label={t('archive.jobStatus')}
          value={running ? t('archive.status.running') : status?.last_run ? t('archive.idle') : t('archive.notRun')}
          tone={running ? 'warning' : 'success'}
          hint={
            status?.last_run
              ? `上次 ${formatRelative(status.last_run.finished_at ?? status.last_run.started_at)} · 搬运 ${formatNumber(status.last_run.moved_rows)} 行`
              : t('archive.noRuns')
          }
        />
        <StatCard
          label={t('archive.hotRows')}
          value={formatNumber(status?.hot_rows ?? null)}
          hint={`占用 ${formatBytes(status?.hot_bytes ?? null)}`}
        />
        <StatCard
          label={t('archive.oldestMessage')}
          value={status?.oldest_message_at ? formatRelative(status.oldest_message_at) : '—'}
          hint={status?.oldest_message_at ? formatDateTime(status.oldest_message_at) : t('archive.hotEmpty')}
          tone={(status?.lag_days ?? 0) > 7 ? 'danger' : 'default'}
        />
        <StatCard
          label={t('archive.coldReach')}
          value={status?.cold_db.reachable ? t('common.normal') : t('dashboard.unreachable')}
          tone={status?.cold_db.reachable ? 'success' : 'danger'}
          hint={status?.cold_db.reachable ? `延迟 ${status.cold_db.latency_ms} ms` : status?.cold_db.error ?? t('archive.coldError')}
        />
      </div>

      <div className="card mb-4 card-pad">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h2 className="text-table font-semibold text-text-primary">{t('archive.policy')}</h2>
            <p className="text-body-sm text-text-tertiary">
              策略来自 <span className="code">/etc/opendiscord/policy.json</span>，改完 <span className="code">systemctl reload opendiscord</span> 热生效。
            </p>
          </div>
          <dl className="flex flex-wrap gap-x-6 gap-y-2 text-body-sm">
            <div className="flex items-center gap-2">
              <dt className="text-text-tertiary">{t('archive.retentionDays')}</dt>
              <dd className="nums font-semibold text-text-primary">{status?.policy.hot_retention_days ?? '—'}</dd>
            </div>
            <div className="flex items-center gap-2">
              <dt className="text-text-tertiary">cron</dt>
              <dd className="code">{status?.policy.cron ?? '—'}</dd>
            </div>
            <div className="flex items-center gap-2">
              <dt className="text-text-tertiary">{t('archive.batchSize')}</dt>
              <dd className="nums font-semibold text-text-primary">{status?.policy.batch_size ?? '—'}</dd>
            </div>
            <div className="flex items-center gap-2">
              <dt className="text-text-tertiary">{t('archive.lag')}</dt>
              <dd className={cn('nums font-semibold', (status?.lag_days ?? 0) > 0 ? 'text-warning-700' : 'text-success-600')}>
                {status?.lag_days ?? 0} 天
              </dd>
            </div>
          </dl>
        </div>
        {(status?.lag_days ?? 0) > 0 && (
          <p className="mt-3 flex items-start gap-2 rounded-sm bg-warning-50 px-3 py-2 text-body-sm text-warning-700">
            <IconAlert size={16} className="mt-0.5 shrink-0" />
            热库存在早于截止线的数据（{status?.lag_days} 天）。检查作业是否被频繁 409 打断，或冷库写入延迟。
          </p>
        )}
      </div>

      <h2 className="mb-2 text-body-sm font-semibold uppercase tracking-wide text-text-tertiary">{t('archive.recentRuns')}</h2>
      <DataTable<ArchiveRun>
        columns={columns}
        rows={runs}
        rowKey={(r) => String(r.id)}
        loading={loading}
        error={null}
        emptyText={t('archive.empty.title')}
        footerLeft={t('archive.footer')}
      />

      <ConfirmDialog
        open={runOpen}
        title={t('archive.trigger')}
        description={t('archive.confirm.desc')}
        tone="primary"
        confirmLabel={t('archive.confirm.action')}
        busy={busy || previewLoading}
        error={actionError ?? previewError}
        onCancel={() => {
          setRunOpen(false);
          setPreview(null);
        }}
        onConfirm={async () => {
          setBusy(true);
          setActionError(null);
          try {
            await runArchive({});
            setRunOpen(false);
            setPreview(null);
            setReloadKey((k) => k + 1);
            fireFlash();
          } catch (err) {
            setActionError(errorMessage(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <div className="space-y-3">
          <DryRunPreview result={preview} loading={previewLoading} error={previewError} onRetry={() => void dryRun()} compact />
          <p className="text-body-sm text-text-tertiary">
            cutoff 由策略决定（{status?.policy.hot_retention_days ?? '—'} 天）。若上一轮仍在运行，本次会被服务端以 409 拒绝。
          </p>
        </div>
      </ConfirmDialog>
    </>
  );
}

function StatCard({
  label,
  value,
  hint,
  tone = 'default',
}: {
  label: string;
  value: string;
  hint?: string;
  tone?: 'default' | 'success' | 'warning' | 'danger';
}) {
  const valueClass =
    tone === 'success'
      ? 'text-success-600'
      : tone === 'warning'
        ? 'text-warning-700'
        : tone === 'danger'
          ? 'text-danger-600'
          : 'text-text-primary';
  return (
    <div className="card card-pad">
      <div className="text-body-sm text-text-tertiary">{label}</div>
      <div className={cn('mt-1 text-display font-bold', valueClass)}>{value}</div>
      {hint && <div className="mt-1 truncate text-caption text-text-tertiary">{hint}</div>}
    </div>
  );
}
