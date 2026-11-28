/** 管理台 · 审计：按 since_id 游标顺序读取（journalctl 语义）。 */

import { useEffect, useState } from 'react';
import { Button } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import { adminAudit } from '@/core/domain/admin';
import type { AdminAuditRecord } from '@/core/domain/admin';

export function AdminAudit() {
  const admin = useAdminStore();
  const [rows, setRows] = useState<AdminAuditRecord[]>([]);
  const [sinceId, setSinceId] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = async (from: number) => {
    setError(null);
    try {
      const result = await adminAudit(admin.context(), { since_id: from, limit: 200 });
      setRows(result.items ?? []);
      setHasMore(Boolean(result.has_more));
      const next = result.next_since_id ?? from;
      setSinceId(next);
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败');
    }
  };

  useEffect(() => {
    void load(0);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <>
      <AdminSection
        title="审计日志"
        actions={
          <Button small icon="refresh" onClick={() => void load(sinceId)}>
            继续加载
          </Button>
        }
      />
      <div className="uf-page__body">
        {error ? <div className="uf-toast uf-toast--error">{error}</div> : null}
        <div className="uf-card">
          {rows.length === 0 ? <div className="uf-faint">暂无审计记录。</div> : null}
          {rows.map((row) => (
            <div key={String(row.id)} className="uf-row" style={{ marginTop: 'var(--sp-2)' }}>
              <span className="uf-faint" style={{ width: '4rem' }}>
                #{String(row.id ?? '')}
              </span>
              <span className="uf-grow truncate">{String(row.action ?? '')}</span>
              <span className="uf-faint truncate">{String(row.actor ?? '')}</span>
            </div>
          ))}
          {hasMore ? <div className="uf-faint">还有更多，点“继续加载”。</div> : null}
        </div>
      </div>
    </>
  );
}
