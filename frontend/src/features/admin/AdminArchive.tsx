/** 管理台 · 归档：查看账本状态，手动触发（默认 dry-run）。 */

import { useEffect, useState } from 'react';
import { Button } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import { adminArchiveStatus, adminRunArchive } from '@/core/domain/admin';

export function AdminArchive() {
  const admin = useAdminStore();
  const [status, setStatus] = useState<string>('');
  const [result, setResult] = useState<string>('');
  const [error, setError] = useState<string | null>(null);

  const load = async () => {
    setError(null);
    try {
      setStatus(JSON.stringify(await adminArchiveStatus(admin.context()), null, 2));
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败');
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <>
      <AdminSection
        title="归档"
        actions={
          <Button small icon="refresh" onClick={() => void load()}>
            刷新
          </Button>
        }
      />
      <div className="uf-page__body">
        {error ? <div className="uf-toast uf-toast--error">{error}</div> : null}
        <div className="uf-card">
          <h3 className="uf-card__title">账本状态</h3>
          <pre className="uf-pre">{status || '（空）'}</pre>
          <div className="uf-row" style={{ marginTop: 'var(--sp-3)' }}>
            <Button
              onClick={() => {
                void adminRunArchive(admin.context(), true)
                  .then((value) => setResult(JSON.stringify(value, null, 2)))
                  .catch((err) => setError(err instanceof Error ? err.message : '执行失败'));
              }}
            >
              试运行（dry-run）
            </Button>
            <Button
              variant="danger"
              onClick={() => {
                void adminRunArchive(admin.context(), false)
                  .then((value) => setResult(JSON.stringify(value, null, 2)))
                  .catch((err) => setError(err instanceof Error ? err.message : '执行失败'));
              }}
            >
              立即执行
            </Button>
          </div>
        </div>
        {result ? (
          <div className="uf-card">
            <h3 className="uf-card__title">运行结果</h3>
            <pre className="uf-pre">{result}</pre>
          </div>
        ) : null}
      </div>
    </>
  );
}
