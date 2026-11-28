/** 管理台 · 举报队列：查看、认领、处置。 */

import { useEffect, useState } from 'react';
import { Button, Field, TextInput } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import { adminHandleReport, adminListReports } from '@/core/domain/admin';
import type { AdminReportRecord } from '@/core/domain/admin';

export function AdminReports() {
  const admin = useAdminStore();
  const [rows, setRows] = useState<AdminReportRecord[]>([]);
  const [status, setStatus] = useState('');
  const [note, setNote] = useState('');
  const [error, setError] = useState<string | null>(null);

  const load = async () => {
    setError(null);
    try {
      const page = await adminListReports(admin.context(), {
        status: status === '' ? undefined : Number(status),
        limit: 50,
      });
      setRows(page.items ?? []);
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败');
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const handle = (id: number, action: 'claim' | 'resolve' | 'reject') => {
    void adminHandleReport(admin.context(), id, action, note || undefined)
      .then(() => load())
      .catch((err) => setError(err instanceof Error ? err.message : '操作失败'));
  };

  return (
    <>
      <AdminSection
        title="举报队列"
        actions={
          <Button small icon="refresh" onClick={() => void load()}>
            刷新
          </Button>
        }
      />
      <div className="uf-page__body">
        {error ? <div className="uf-toast uf-toast--error">{error}</div> : null}
        <div className="uf-card">
          <Field label="状态过滤" hint="-1..3，留空为全部">
            {(id) => (
              <TextInput
                id={id}
                value={status}
                inputMode="numeric"
                onChange={(event) => setStatus(event.target.value)}
              />
            )}
          </Field>
          <Field label="处置备注">
            {(id) => (
              <TextInput id={id} value={note} onChange={(event) => setNote(event.target.value)} />
            )}
          </Field>
        </div>

        <div className="uf-card">
          {rows.length === 0 ? <div className="uf-faint">暂无举报。</div> : null}
          {rows.map((row) => (
            <div key={String(row.id)} className="uf-row" style={{ marginTop: 'var(--sp-3)' }}>
              <div className="uf-col uf-grow">
                <span>举报 #{String(row.id ?? '')}</span>
                <span className="uf-faint truncate">{String(row.reason ?? '')}</span>
              </div>
              <Button small onClick={() => handle(Number(row.id), 'claim')}>
                认领
              </Button>
              <Button small variant="primary" onClick={() => handle(Number(row.id), 'resolve')}>
                处置
              </Button>
              <Button small variant="danger" onClick={() => handle(Number(row.id), 'reject')}>
                驳回
              </Button>
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
