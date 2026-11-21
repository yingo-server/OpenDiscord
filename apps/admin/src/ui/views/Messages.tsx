import { useCallback, useEffect, useMemo, useState } from 'react';
import {
  recallMessage,
  searchConversations,
  searchMessages,
  type ConvOption,
  type MessageHit,
} from '../../api/messages';
import type { DryRunResult, Page } from '../../api/types';
import { errorMessage, useDebouncedValue, useTransientFlag } from '../../lib/hooks';
import { cn, formatBytes, formatDateTime, formatRelative } from '../../lib/format';
import { ConfirmDialog } from '../components/ConfirmDialog';
import { DataTable, FilterBar, SearchInput, type Column } from '../components/DataTable';
import { DryRunPreview, ImpactList } from '../components/DryRunPreview';
import { Modal } from '../components/Modal';
import { PageHeader } from '../components/PageHeader';
import { StatusTag } from '../components/StatusTag';
import { IconRefresh, IconSearch, IconX } from '../components/icons';
import { t } from '../i18n';

export function Messages() {
  const [q, setQ] = useState('');
  const debouncedQ = useDebouncedValue(q, 300);
  const [conv, setConv] = useState<ConvOption | null>(null);
  const [sender, setSender] = useState('');
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');
  const [page, setPage] = useState(1);
  const [perPage, setPerPage] = useState(20);

  const [data, setData] = useState<Page<MessageHit> | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [searched, setSearched] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);

  const [active, setActive] = useState<MessageHit | null>(null);
  const [recallTarget, setRecallTarget] = useState<MessageHit | null>(null);
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [flash, fireFlash] = useTransientFlag();

  // 至少给一个条件才查询：避免无索引全表扫
  const canSearch = debouncedQ.trim().length >= 2 || conv !== null || sender.trim().length > 0;
  const queryKey = useMemo(
    () => JSON.stringify({ q: debouncedQ, conv: conv?.id, sender, from, to, page, perPage }),
    [debouncedQ, conv, sender, from, to, page, perPage],
  );

  useEffect(() => {
    setPage(1);
  }, [debouncedQ, conv, sender, from, to, perPage]);

  useEffect(() => {
    if (!canSearch) {
      setData(null);
      setSearched(false);
      return undefined;
    }
    const controller = new AbortController();
    setLoading(true);
    searchMessages(
      {
        q: debouncedQ.trim() || undefined,
        conv: conv?.id,
        sender: sender.trim() || undefined,
        from: from || undefined,
        to: to || undefined,
        page,
        per_page: perPage,
      },
      controller.signal,
    )
      .then((res) => {
        setData(res);
        setError(null);
        setSearched(true);
      })
      .catch((err) => {
        if ((err as Error).name === 'AbortError') return;
        setError(errorMessage(err));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [queryKey, reloadKey]);

  const columns: Column<MessageHit>[] = [
    {
      key: 'content',
      header: t('messages.content'),
      width: 420,
      render: (m) => (
        <div className="min-w-0 py-1">
          <p className={cn('line-clamp-2 whitespace-pre-wrap text-table', m.state === 'recalled' ? 'text-text-tertiary line-through' : 'text-text-primary')}>
            {m.content || t('common.noText')}
          </p>
          {m.attachments.length > 0 && (
            <div className="mt-1 flex flex-wrap gap-1">
              {m.attachments.map((a) => (
                <span key={a.name} className="tag bg-bg-surface-2 text-text-secondary">
                  {a.kind} · {a.name} · <span className="nums">{formatBytes(a.size)}</span>
                </span>
              ))}
            </div>
          )}
          <div className="nums text-caption text-text-tertiary">msg #{m.msg_id}</div>
        </div>
      ),
    },
    {
      key: 'conv',
      header: t('messages.conversation'),
      width: 220,
      render: (m) => (
        <div className="min-w-0">
          <div className="truncate text-body-sm text-text-primary">{m.conv_name}</div>
          <div className="nums text-caption text-text-tertiary">#{m.conv_id}</div>
        </div>
      ),
    },
    {
      key: 'sender',
      header: t('messages.sender'),
      width: 160,
      render: (m) => (
        <div className="min-w-0">
          <div className="truncate text-body-sm text-text-primary">{m.sender_name}</div>
          <div className="nums text-caption text-text-tertiary">#{m.sender_id}</div>
        </div>
      ),
    },
    {
      key: 'sent_at',
      header: t('messages.sentAt'),
      width: 170,
      render: (m) => (
        <div>
          <div className="nums text-body-sm text-text-secondary">{formatDateTime(m.sent_at)}</div>
          <div className="text-caption text-text-tertiary">{formatRelative(m.sent_at)}</div>
        </div>
      ),
    },
    {
      key: 'state',
      header: t('common.status'),
      width: 110,
      render: (m) => (
        <StatusTag tone={m.state === 'normal' ? 'success' : m.state === 'recalled' ? 'muted' : 'danger'}>
          {m.state === 'normal' ? t('messages.state.normal') : m.state === 'recalled' ? t('messages.state.recalled') : t('messages.state.deleted')}
        </StatusTag>
      ),
    },
    {
      key: 'actions',
      header: t('common.actions'),
      width: 140,
      align: 'right',
      render: (m) => (
        <div className="flex items-center justify-end gap-1">
          <button type="button" className="btn btn-ghost btn-sm" onClick={() => setActive(m)}>
            查看
          </button>
          <button
            type="button"
            className="btn btn-danger-ghost btn-sm"
            disabled={m.state !== 'normal'}
            onClick={() => {
              setActionError(null);
              setRecallTarget(m);
            }}
          >
            撤回
          </button>
        </div>
      ),
    },
  ];

  return (
    <>
      <PageHeader
        title={t('messages.title')}
        description={t('messages.description')}
        meta={
          <>
            {searched && (
              <span>
                {t('messages.hits')} <span className="nums font-semibold text-text-secondary">{data?.total ?? 0}</span> 条
              </span>
            )}
            {flash && <span className="text-success-600">{t('messages.state.recalled')}</span>}
            {actionError && <span className="text-danger-600">{actionError}</span>}
          </>
        }
        actions={
          <button
            type="button"
            className="btn btn-secondary btn-sm"
            onClick={() => setReloadKey((k) => k + 1)}
            disabled={!canSearch}
          >
            <IconRefresh size={14} /> 重新检索
          </button>
        }
      />

      <FilterBar
        onReset={() => {
          setQ('');
          setConv(null);
          setSender('');
          setFrom('');
          setTo('');
          setPage(1);
        }}
      >
        <div>
          <label className="label" htmlFor="msg-q">
            关键词（≥2 字）
          </label>
          <SearchInput value={q} onChange={setQ} placeholder="消息内容关键词" width={280} />
        </div>
        <ConvPicker value={conv} onChange={setConv} />
        <div>
          <label className="label" htmlFor="msg-sender">
            发送者 ID
          </label>
          <input
            id="msg-sender"
            className="input nums w-[140px]"
            value={sender}
            onChange={(event) => setSender(event.target.value)}
            placeholder={t('messages.optional')}
          />
        </div>
        <div>
          <label className="label" htmlFor="msg-from">
            起始日期
          </label>
          <input id="msg-from" type="date" className="input w-[160px]" value={from} onChange={(e) => setFrom(e.target.value)} />
        </div>
        <div>
          <label className="label" htmlFor="msg-to">
            结束日期
          </label>
          <input id="msg-to" type="date" className="input w-[160px]" value={to} onChange={(e) => setTo(e.target.value)} />
        </div>
      </FilterBar>

      {!canSearch && !searched ? (
        <div className="flex flex-col items-center gap-3 rounded-md border border-dashed border-border-default px-6 py-16 text-center">
          <IconSearch size={28} className="text-text-tertiary" />
          <p className="text-table text-text-secondary">{t('messages.conditionHint')}</p>
          <p className="text-body-sm text-text-tertiary">{t('messages.conditionRisk')}</p>
        </div>
      ) : (
        <DataTable<MessageHit>
          columns={columns}
          rows={data?.items ?? []}
          rowKey={(m) => m.msg_id}
          loading={loading}
          error={error}
          emptyText="没有命中的消息"
          pagination={{
            page,
            perPage,
            total: data?.total ?? 0,
            onPageChange: setPage,
            onPerPageChange: setPerPage,
          }}
          footerLeft={t('messages.auditHint')}
        />
      )}

      <Modal
        open={active !== null}
        title={t('messages.detail')}
        width={640}
        onClose={() => setActive(null)}
        description={active ? `msg #${active.msg_id} · 会话 #${active.conv_id}` : undefined}
        footer={
          <button type="button" className="btn btn-secondary" onClick={() => setActive(null)}>
            关闭
          </button>
        }
      >
        {active && (
          <div className="space-y-3">
            <div className="rounded-md bg-bg-surface-2 px-3 py-2">
              <p className="whitespace-pre-wrap text-body text-text-primary">{active.content || t('common.noText')}</p>
            </div>
            <dl className="grid grid-cols-2 gap-x-4 gap-y-2 text-body-sm">
              <div className="flex justify-between">
                <dt className="text-text-tertiary">{t('messages.sender')}</dt>
                <dd className="nums text-text-primary">{active.sender_name} #{active.sender_id}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-tertiary">{t('messages.sentAt')}</dt>
                <dd className="nums text-text-primary">{formatDateTime(active.sent_at)}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-tertiary">{t('messages.conversation')}</dt>
                <dd className="text-text-primary">{active.conv_name}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-text-tertiary">{t('common.status')}</dt>
                <dd><StatusTag tone={active.state === 'normal' ? 'success' : 'muted'}>{active.state}</StatusTag></dd>
              </div>
            </dl>
          </div>
        )}
      </Modal>

      <RecallDialog
        target={recallTarget}
        busy={busy}
        error={actionError}
        onClose={() => setRecallTarget(null)}
        onConfirm={async (body) => {
          const target = recallTarget;
          if (!target) return;
          setBusy(true);
          setActionError(null);
          try {
            await recallMessage(target.msg_id, body);
            setRecallTarget(null);
            setActive(null);
            setReloadKey((k) => k + 1);
            fireFlash();
          } catch (err) {
            setActionError(errorMessage(err));
          } finally {
            setBusy(false);
          }
        }}
      />
    </>
  );
}

/** 会话选择器：输入即查（防抖 300ms），选中后显示为可清除的胶囊。 */
function ConvPicker({ value, onChange }: { value: ConvOption | null; onChange: (next: ConvOption | null) => void }) {
  const [text, setText] = useState('');
  const debounced = useDebouncedValue(text, 300);
  const [options, setOptions] = useState<ConvOption[]>([]);
  const [open, setOpen] = useState(false);

  useEffect(() => {
    if (!debounced.trim()) {
      setOptions([]);
      return undefined;
    }
    const controller = new AbortController();
    searchConversations(debounced.trim(), controller.signal)
      .then((res) => setOptions(res))
      .catch(() => setOptions([]));
    return () => controller.abort();
  }, [debounced]);

  if (value) {
    return (
      <div>
        <span className="label">{t('messages.conversation')}</span>
        <div className="flex h-control-md items-center gap-2 rounded-sm border border-primary-300 bg-primary-50 px-2">
          <span className="max-w-[200px] truncate text-body-sm text-text-primary">{value.name}</span>
          <span className="nums text-caption text-text-tertiary">#{value.id}</span>
          <button type="button" className="text-text-tertiary hover:text-text-primary" onClick={() => onChange(null)} aria-label={t('messages.clearConvFilter')}>
            <IconX size={14} />
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="relative">
      <label className="label" htmlFor="msg-conv">
        会话
      </label>
      <input
        id="msg-conv"
        className="input w-[220px]"
        value={text}
        placeholder={t('messages.convPlaceholder')}
        onChange={(event) => {
          setText(event.target.value);
          setOpen(true);
        }}
        onFocus={() => setOpen(true)}
        onBlur={() => window.setTimeout(() => setOpen(false), 150)}
      />
      {open && options.length > 0 && (
        <ul className="absolute z-menu mt-1 max-h-64 w-[280px] overflow-y-auto rounded-md border border-border-default bg-bg-elevated py-1 shadow-md">
          {options.map((opt) => (
            <li key={opt.id}>
              <button
                type="button"
                className="flex w-full items-center justify-between px-3 py-1.5 text-left hover:bg-bg-surface-2"
                onMouseDown={(event) => event.preventDefault()}
                onClick={() => {
                  onChange(opt);
                  setText('');
                  setOpen(false);
                }}
              >
                <span className="truncate text-body-sm text-text-primary">{opt.name}</span>
                <span className="nums text-caption text-text-tertiary">{opt.member_count} 人</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function RecallDialog({
  target,
  busy,
  error,
  onClose,
  onConfirm,
}: {
  target: MessageHit | null;
  busy: boolean;
  error: string | null;
  onClose: () => void;
  onConfirm: (body: { reason: string; all_devices: boolean; notify_sender: boolean }) => Promise<void>;
}) {
  const [reason, setReason] = useState('');
  const [allDevices, setAllDevices] = useState(true);
  const [notify, setNotify] = useState(true);
  const [preview, setPreview] = useState<DryRunResult | null>(null);
  const [previewLoading, setPreviewLoading] = useState(false);
  const [previewError, setPreviewError] = useState<string | null>(null);

  useEffect(() => {
    if (target) {
      setReason('');
      setAllDevices(true);
      setNotify(true);
      setPreview(null);
      setPreviewError(null);
    }
  }, [target?.msg_id]);

  const loadPreview = useCallback(async () => {
    if (!target) return;
    setPreviewLoading(true);
    setPreviewError(null);
    try {
      const res = await recallMessage(target.msg_id, { reason, all_devices: allDevices, notify_sender: notify }, { dryRun: true });
      setPreview('dry_run' in res ? res : null);
    } catch (err) {
      setPreviewError(errorMessage(err));
    } finally {
      setPreviewLoading(false);
    }
  }, [target, reason, allDevices, notify]);

  return (
    <ConfirmDialog
      open={target !== null}
      title={t('messages.recall')}
      description={target ? `msg #${target.msg_id} · ${target.conv_name}` : undefined}
      confirmLabel={t('messages.recallConfirm')}
      busy={busy}
      error={error}
      onCancel={onClose}
      onConfirm={() => void onConfirm({ reason, all_devices: allDevices, notify_sender: notify })}
    >
      <div className="space-y-3">
        <ImpactList
          items={[
            { label: t('messages.sender'), value: target ? `${target.sender_name} #${target.sender_id}` : '—' },
            { label: t('messages.sentAt'), value: formatDateTime(target?.sent_at) },
          ]}
        />
        <div>
          <label className="label" htmlFor="recall-reason">
            撤回原因（写入审计）
          </label>
          <textarea
            id="recall-reason"
            className="textarea"
            rows={3}
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            placeholder={t('messages.recallReasonPlaceholder')}
          />
        </div>
        <label className="flex items-center gap-2 text-body-sm text-text-secondary">
          <input type="checkbox" className="h-4 w-4 accent-primary-500" checked={allDevices} onChange={(e) => setAllDevices(e.target.checked)} />
          全端撤回（含已下发到客户端的副本）
        </label>
        <label className="flex items-center gap-2 text-body-sm text-text-secondary">
          <input type="checkbox" className="h-4 w-4 accent-primary-500" checked={notify} onChange={(e) => setNotify(e.target.checked)} />
          通知发送者
        </label>
        <div className="flex justify-end">
          <button type="button" className="btn btn-ghost btn-sm" onClick={() => void loadPreview()} disabled={previewLoading}>
            {previewLoading ? t('common.estimating') : t('common.estimateImpact')}
          </button>
        </div>
        <DryRunPreview result={preview} loading={previewLoading} error={previewError} compact />
      </div>
    </ConfirmDialog>
  );
}
