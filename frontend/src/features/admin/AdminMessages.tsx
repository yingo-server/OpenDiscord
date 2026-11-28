/** 管理台 · 消息：检索与召回。 */

import { useEffect, useState } from 'react';
import { Button, Field, TextInput } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import { adminRecallMessage, adminSearchMessages } from '@/core/domain/admin';
import type { AdminMessageRecord } from '@/core/domain/admin';

export function AdminMessages() {
  const admin = useAdminStore();
  const [query, setQuery] = useState('');
  const [convId, setConvId] = useState('');
  const [rows, setRows] = useState<AdminMessageRecord[]>([]);
  const [error, setError] = useState<string | null>(null);

  const load = async () => {
    setError(null);
    try {
      const page = await adminSearchMessages(admin.context(), {
        q: query || undefined,
        conv_id: convId === '' ? undefined : Number(convId),
        limit: 50,
      });
      setRows(page.items ?? []);
    } catch (err) {
      setError(err instanceof Error ? err.message : '搜索失败');
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <>
      <AdminSection
        title="消息检索"
        actions={
          <Button small icon="refresh" onClick={() => void load()}>
            刷新
          </Button>
        }
      />
      <div className="uf-page__body">
        {error ? <div className="uf-toast uf-toast--error">{error}</div> : null}
        <div className="uf-card">
          <div className="uf-row" style={{ flexWrap: 'wrap' }}>
            <Field label="关键字">
              {(id) => (
                <TextInput
                  id={id}
                  value={query}
                  placeholder="正文片段"
                  onChange={(event) => setQuery(event.target.value)}
                />
              )}
            </Field>
            <Field label="会话 ID">
              {(id) => (
                <TextInput
                  id={id}
                  value={convId}
                  inputMode="numeric"
                  onChange={(event) => setConvId(event.target.value)}
                />
              )}
            </Field>
            <Button style={{ alignSelf: 'flex-end' }} onClick={() => void load()}>
              搜索
            </Button>
          </div>
        </div>

        <div className="uf-card">
          {rows.length === 0 ? <div className="uf-faint">没有匹配的消息。</div> : null}
          {rows.map((row) => (
            <div key={String(row.id ?? row.msg_id)} className="uf-row" style={{ marginTop: 'var(--sp-3)' }}>
              <div className="uf-col uf-grow">
                <span className="truncate">{String(row.text ?? '')}</span>
                <span className="uf-faint">
                  会话 {String(row.conv_id ?? '')} · 发送者 {String(row.sender_id ?? '')}
                </span>
              </div>
              <Button
                small
                variant="danger"
                onClick={() => {
                  void adminRecallMessage(admin.context(), Number(row.id ?? row.msg_id), '管理员召回')
                    .then(() => load())
                    .catch((err) => setError(err instanceof Error ? err.message : '召回失败'));
                }}
              >
                召回
              </Button>
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
