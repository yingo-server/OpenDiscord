/** 管理台 · 特性开关：读取与写入（PUT /api/v1/flags）。 */

import { useEffect, useState } from 'react';
import { Button, TextInput } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import { adminListFlags, adminSetFlags } from '@/core/domain/admin';
import type { AdminFlagRecord } from '@/core/domain/admin';

export function AdminFlags() {
  const admin = useAdminStore();
  const [rows, setRows] = useState<AdminFlagRecord[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [drafts, setDrafts] = useState<Record<string, string>>({});

  const load = async () => {
    setError(null);
    try {
      const result = await adminListFlags(admin.context());
      setRows(result.items);
      setDrafts(Object.fromEntries(result.items.map((f) => [f.key, f.value])));
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败');
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const save = (key: string) => {
    void adminSetFlags(admin.context(), { key, value: drafts[key] ?? '' })
      .then(() => load())
      .catch((err) => setError(err instanceof Error ? err.message : '写入失败'));
  };

  return (
    <>
      <AdminSection
        title="特性开关"
        actions={
          <Button small icon="refresh" onClick={() => void load()}>
            刷新
          </Button>
        }
      />
      <div className="uf-page__body">
        {error ? <div className="uf-toast uf-toast--error">{error}</div> : null}
        {rows.length === 0 ? <div className="uf-card uf-faint">暂无开关数据。</div> : null}
        {rows.map((flag) => (
          <div key={flag.key} className="uf-card">
            <div className="uf-row">
              <strong className="uf-grow">{flag.key}</strong>
              <span className="uf-faint">v{flag.version}</span>
            </div>
            <div className="uf-row" style={{ marginTop: 'var(--sp-2)' }}>
              <TextInput
                value={drafts[flag.key] ?? ''}
                aria-label={`${flag.key} 的值`}
                onChange={(event) =>
                  setDrafts((prev) => ({ ...prev, [flag.key]: event.target.value }))
                }
              />
              <Button small onClick={() => save(flag.key)}>
                写入
              </Button>
            </div>
          </div>
        ))}
      </div>
    </>
  );
}
