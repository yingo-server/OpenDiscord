import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  fetchMetricsText,
  firstMetric,
  httpErrorRate,
  parseMetrics,
  type MetricFamily,
} from '../../api/metrics';
import { errorMessage, useTransientFlag } from '../../lib/hooks';
import { cn, formatBytes, formatClock, formatNumber, formatPercent } from '../../lib/format';
import { PageHeader } from '../components/PageHeader';
import { StatusTag } from '../components/StatusTag';
import { IconAlert, IconGauge, IconRefresh } from '../components/icons';
import { FilterBar, SearchInput } from '../components/DataTable';
import { t } from '../i18n';

const REFRESH_MS = 5000;

export function Metrics() {
  const [text, setText] = useState('');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);
  const [auto, setAuto] = useState(true);
  const [q, setQ] = useState('');
  const [flash, fireFlash] = useTransientFlag();

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    try {
      const body = await fetchMetricsText(signal);
      setText(body);
      setUpdatedAt(new Date());
      setError(null);
    } catch (err) {
      if ((err as Error).name === 'AbortError') return;
      setError(errorMessage(err));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  useEffect(() => {
    if (!auto) return undefined;
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void load();
    }, REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [auto, load]);

  const families = useMemo(() => (text ? parseMetrics(text) : []), [text]);

  const connections = firstMetric(families, [
    'opendiscord_ws_connections_active',
    'ws_connections_active',
    'gateway_connections_active',
    'opendiscord_connections_active',
  ]);
  const queue = firstMetric(families, ['writech_queue_depth', 'opendiscord_writech_queue_depth'], 'max');
  const errorRate = httpErrorRate(families);
  const chainBroken = firstMetric(families, ['audit_chain_broken', 'opendiscord_audit_chain_broken'], 'max');
  const goroutines = firstMetric(families, ['go_goroutines', 'opendiscord_goroutines'], 'max');
  const rss = firstMetric(families, ['process_resident_memory_bytes', 'opendiscord_resident_memory_bytes'], 'max');
  const rateLimited = firstMetric(families, ['gateway_ratelimit_dropped_total', 'opendiscord_ratelimit_dropped_total']);
  const busy = firstMetric(families, ['writech_busy_total', 'opendiscord_writech_busy_total']);

  const filtered = useMemo(() => {
    const term = q.trim().toLowerCase();
    if (!term) return families;
    return families.filter((f) => f.name.toLowerCase().includes(term) || f.help.toLowerCase().includes(term));
  }, [families, q]);

  const queueValue = queue?.value ?? 0;
  const queueTone = queueValue > 64 ? 'danger' : queueValue > 32 ? 'warning' : 'success';

  return (
    <>
      <PageHeader
        title={t('metrics.title')}
        description={t('metrics.description')}
        meta={
          <>
            <span>
              更新于 <span className="nums">{updatedAt ? formatClock(updatedAt) : '—'}</span>
            </span>
            {loading && <span className="text-text-tertiary">拉取中…</span>}
            {flash && <span className="text-success-600">{t('common.updated')}</span>}
            <span className="text-text-tertiary">共 {families.length} 个指标族</span>
          </>
        }
        actions={
          <>
            <label className="flex items-center gap-2 text-body-sm text-text-secondary">
              <input type="checkbox" className="h-4 w-4 accent-primary-500" checked={auto} onChange={(e) => setAuto(e.target.checked)} />
              5s 自动刷新
            </label>
            <button
              type="button"
              className="btn btn-secondary btn-sm"
              onClick={() => {
                void load();
                fireFlash();
              }}
            >
              <IconRefresh size={14} /> 立即刷新
            </button>
          </>
        }
      />

      {chainBroken && chainBroken.value > 0 && (
        <div className="mb-4 flex items-start gap-2 rounded-md border border-danger-300 bg-danger-50 px-3 py-2 text-body-sm text-danger-700">
          <IconAlert size={16} className="mt-0.5 shrink-0" />
          <div>
            <strong className="font-semibold">{t('metrics.auditBroken.title')}</strong>
            <p>audit_chain_broken=1。立即冻结写入、保全现场并通知安全负责人（security-spec §6.4.2）。</p>
          </div>
        </div>
      )}

      {error && (
        <div className="mb-4 flex items-start gap-2 rounded-md bg-danger-50 px-3 py-2 text-body-sm text-danger-700">
          <IconAlert size={16} className="mt-0.5 shrink-0" />
          <div>
            <p>{error}</p>
            <p className="mt-1 text-text-secondary">
              排查：确认 chatd 的 metrics 监听已启用，且 Caddy 对 /metrics 做了 IP 白名单（不应公网暴露）。
            </p>
          </div>
        </div>
      )}

      <div className="mb-4 grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
        <MetricCard
          label={t('metrics.connections')}
          value={connections ? formatNumber(connections.value) : '—'}
          unit={connections ? '条' : undefined}
          hint={connections ? connections.name : t('metrics.missingConnections')}
          icon
        />
        <MetricCard
          label={t('metrics.queueDepth')}
          value={queue ? formatNumber(queueValue) : '—'}
          unit={queue ? `/ 128` : undefined}
          hint={queue ? t('metrics.queueHint') : t('metrics.missingQueue')}
          tone={queue ? queueTone : 'default'}
          progress={queue ? Math.min(1, queueValue / 128) : undefined}
        />
        <MetricCard
          label={t('metrics.http5xx')}
          value={formatPercent(errorRate)}
          hint={t('metrics.http5xxHint')}
          tone={errorRate === null ? 'default' : errorRate > 0.01 ? 'danger' : errorRate > 0.001 ? 'warning' : 'success'}
        />
        <MetricCard
          label={t('metrics.rateDrops')}
          value={rateLimited ? formatNumber(rateLimited.value) : '—'}
          hint="gateway_ratelimit_dropped_total"
        />
        <MetricCard
          label={t('metrics.busy')}
          value={busy ? formatNumber(busy.value) : '—'}
          hint={t('metrics.busyHint')}
        />
        <MetricCard
          label={t('metrics.memory')}
          value={rss ? formatBytes(rss.value) : '—'}
          hint={goroutines ? `${formatNumber(goroutines.value)} goroutines` : 'process_resident_memory_bytes'}
        />
      </div>

      <FilterBar>
        <div>
          <label className="label" htmlFor="metric-q">
            指标过滤
          </label>
          <SearchInput value={q} onChange={setQ} placeholder="按指标名或 HELP 过滤" width={320} />
        </div>
      </FilterBar>

      <div className="card overflow-hidden">
        <table className="tbl">
          <thead>
            <tr>
              <th>{t('metrics.family')}</th>
              <th style={{ width: 110 }}>{t('metrics.type')}</th>
              <th style={{ width: 110 }} className="text-right">
                样本数
              </th>
              <th style={{ width: 160 }} className="text-right">
                合计 / 值
              </th>
              <th>{t('metrics.labels')}</th>
            </tr>
          </thead>
          <tbody>
            {filtered.map((family) => (
              <MetricRow key={family.name} family={family} />
            ))}
            {filtered.length === 0 && (
              <tr>
                <td colSpan={5}>
                  <div className="flex flex-col items-center gap-2 py-10 text-center">
                    <IconGauge size={24} className="text-text-tertiary" />
                    <p className="text-body-sm text-text-tertiary">
                      {families.length === 0 ? t('metrics.emptyAll') : t('metrics.emptyFiltered')}
                    </p>
                  </div>
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}

function MetricCard({
  label,
  value,
  unit,
  hint,
  tone = 'default',
  progress,
  icon = false,
}: {
  label: string;
  value: string;
  unit?: string;
  hint?: string;
  tone?: 'default' | 'success' | 'warning' | 'danger';
  progress?: number;
  icon?: boolean;
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
      <div className="flex items-center justify-between">
        <span className="text-body-sm text-text-tertiary">{label}</span>
        {icon && <IconGauge size={16} className="text-text-tertiary" />}
        {tone !== 'default' && (
          <StatusTag tone={tone === 'success' ? 'success' : tone === 'warning' ? 'warning' : 'danger'}>
            {tone === 'success' ? t('common.normal') : tone === 'warning' ? t('common.attention') : t('common.alert')}
          </StatusTag>
        )}
      </div>
      <div className="mt-1 flex items-baseline gap-1">
        <span className={cn('nums text-display font-bold', valueClass)}>{value}</span>
        {unit && <span className="nums text-body-sm text-text-tertiary">{unit}</span>}
      </div>
      {progress !== undefined && (
        <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-bg-surface-2">
          <div
            className={cn(
              'h-full rounded-full transition-[width] duration-normal',
              tone === 'danger' ? 'bg-danger-500' : tone === 'warning' ? 'bg-warning-500' : 'bg-success-500',
            )}
            style={{ width: `${Math.round(progress * 100)}%` }}
          />
        </div>
      )}
      {hint && <div className="mt-1 truncate text-caption text-text-tertiary" title={hint}>{hint}</div>}
    </div>
  );
}

function MetricRow({ family }: { family: MetricFamily }) {
  const total = family.samples.reduce((n, s) => n + s.value, 0);
  const labelKeys = [...new Set(family.samples.flatMap((s) => Object.keys(s.labels)))];
  return (
    <tr>
      <td>
        <div className="text-table text-text-primary">{family.name}</div>
        {family.help && <div className="line-clamp-1 text-caption text-text-tertiary">{family.help}</div>}
      </td>
      <td>
        <span className="code">{family.type}</span>
      </td>
      <td className="text-right">
        <span className="nums text-body-sm text-text-secondary">{formatNumber(family.samples.length)}</span>
      </td>
      <td className="text-right">
        <span className="nums text-body-sm text-text-primary">
          {family.type === 'counter' || family.type === 'untyped' ? formatNumber(Math.round(total * 100) / 100) : formatNumber(total)}
        </span>
      </td>
      <td>
        <div className="flex flex-wrap gap-1">
          {labelKeys.slice(0, 4).map((key) => (
            <span key={key} className="code">
              {key}
            </span>
          ))}
          {labelKeys.length > 4 && <span className="text-caption text-text-tertiary">+{labelKeys.length - 4}</span>}
        </div>
      </td>
    </tr>
  );
}
