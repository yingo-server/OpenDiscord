/** 管理台 · 广播：先预览（dry_run）再发送。 */

import { useState } from 'react';
import { Button, Field, TextArea } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import { adminBroadcast } from '@/core/domain/admin';

export function AdminBroadcast() {
  const admin = useAdminStore();
  const [text, setText] = useState('');
  const [result, setResult] = useState<string>('');
  const [error, setError] = useState<string | null>(null);

  const run = (dryRun: boolean) => {
    setError(null);
    void adminBroadcast(admin.context(), { text }, dryRun)
      .then((value) => setResult(JSON.stringify(value, null, 2)))
      .catch((err) => setError(err instanceof Error ? err.message : '请求失败'));
  };

  return (
    <>
      <AdminSection title="广播" />
      <div className="uf-page__body">
        {error ? <div className="uf-toast uf-toast--error">{error}</div> : null}
        <div className="uf-card">
          <Field label="广播文案" hint="发送是不可逆的写操作，建议先预览">
            {(id) => (
              <TextArea id={id} rows={4} value={text} onChange={(e) => setText(e.target.value)} />
            )}
          </Field>
          <div className="uf-row" style={{ marginTop: 'var(--sp-3)' }}>
            <Button onClick={() => run(true)} disabled={!text.trim()}>
              预览触达
            </Button>
            <Button variant="danger" onClick={() => run(false)} disabled={!text.trim()}>
              立即发送
            </Button>
          </div>
        </div>
        {result ? (
          <div className="uf-card">
            <h3 className="uf-card__title">结果</h3>
            <pre className="uf-pre">{result}</pre>
          </div>
        ) : null}
      </div>
    </>
  );
}
