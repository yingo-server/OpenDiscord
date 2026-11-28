/** 管理台 · 用户：检索、封禁、解封、踢下线。 */

import { useEffect, useState } from 'react';
import { Button, Field, TextInput } from '@/ui/primitives/controls';
import { AdminSection } from './AdminApp';
import { useAdminStore } from './adminStore';
import {
  adminBanUser,
  adminGetUser,
  adminKickUser,
  adminListUsers,
  adminUnbanUser,
} from '@/core/domain/admin';
import type { AdminUserRecord } from '@/core/domain/admin';

export function AdminUsers() {
  const admin = useAdminStore();
  const [query, setQuery] = useState('');
  const [status, setStatus] = useState('');
  const [rows, setRows] = useState<AdminUserRecord[]>([]);
  const [total, setTotal] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<AdminUserRecord | null>(null);

  const load = async () => {
    setError(null);
    try {
      const page = await adminListUsers(admin.context(), {
        q: query || undefined,
        status: status === '' ? undefined : Number(status),
        limit: 50,
      });
      setRows(page.items ?? []);
      setTotal(page.total ?? 0);
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载失败');
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const idOf = (row: AdminUserRecord): number => Number(row.user_id ?? row.id ?? 0);

  return (
    <>
      <AdminSection
        title={`用户（${total}）`}
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
                  placeholder="昵称 / ID"
                  onChange={(event) => setQuery(event.target.value)}
                />
              )}
            </Field>
            <Field label="状态">
              {(id) => (
                <TextInput
                  id={id}
                  value={status}
                  inputMode="numeric"
                  placeholder="-1..3"
                  onChange={(event) => setStatus(event.target.value)}
                />
              )}
            </Field>
            <Button style={{ alignSelf: 'flex-end' }} onClick={() => void load()}>
              查询
            </Button>
          </div>
        </div>

        <div className="uf-card">
          {rows.length === 0 ? <div className="uf-faint">没有匹配的用户。</div> : null}
          {rows.map((row) => (
            <div key={idOf(row)} className="uf-row" style={{ marginTop: 'var(--sp-3)' }}>
              <div className="uf-col uf-grow">
                <span className="truncate">
                  {String(row.display_name ?? row.username ?? `用户 ${idOf(row)}`)}
                </span>
                <span className="uf-faint">ID {idOf(row)} · 状态 {String(row.status ?? '—')}</span>
              </div>
              <Button
                small
                onClick={() => {
                  void adminGetUser(admin.context(), idOf(row)).then(setSelected).catch(() => undefined);
                }}
              >
                详情
              </Button>
              <Button
                small
                variant="danger"
                onClick={() => {
                  void adminKickUser(admin.context(), idOf(row), '管理员操作').then(() => load());
                }}
              >
                踢下线
              </Button>
              <Button
                small
                variant="danger"
                onClick={() => {
                  void adminBanUser(admin.context(), idOf(row), { level: 1, reason: '管理员操作' }).then(
                    () => load(),
                  );
                }}
              >
                封禁
              </Button>
              <Button
                small
                onClick={() => {
                  void adminUnbanUser(admin.context(), idOf(row), '管理员操作').then(() => load());
                }}
              >
                解封
              </Button>
            </div>
          ))}
        </div>

        {selected ? (
          <div className="uf-card">
            <h3 className="uf-card__title">用户详情</h3>
            <pre className="uf-pre">{JSON.stringify(selected, null, 2)}</pre>
          </div>
        ) : null}
      </div>
    </>
  );
}
