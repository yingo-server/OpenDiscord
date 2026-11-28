/** 管理台概览：服务健康、指标、TURN 自检。 */

import { useEffect, useState } from 'react';
import { Button } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import { adminMetrics, adminTurnCheck } from '@/core/domain/admin';

export function AdminOverview() {
  const admin = useAdminStore();
  const [metrics, setMetrics] = useState('');
  const [turn, setTurn] = useState<string>('');
  const [error, setError] = useState<string | null>(null);

  const load = async () => {
    setError(null);
    try {
      const [text, turnResult] = await Promise.all([
        adminMetrics(admin.context()),
        adminTurnCheck(admin.context()),
      ]);
      setMetrics(text.slice(0, 4000));
      setTurn(JSON.stringify(turnResult, null, 2));
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
        title="概览"
        actions={
          <Button small icon="refresh" onClick={() => void load()}>
            刷新
          </Button>
        }
      />
      <div className="uf-page__body">
        {error ? <div className="uf-toast uf-toast--error">{error}</div> : null}
        <section className="uf-card">
          <h3 className="uf-card__title">Prometheus 指标</h3>
          <pre className="uf-pre">{metrics || '（空）'}</pre>
        </section>
        <section className="uf-card">
          <h3 className="uf-card__title">TURN 连通性</h3>
          <pre className="uf-pre">{turn || '（未检测）'}</pre>
        </section>
      </div>
    </>
  );
}
