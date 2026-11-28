/** 管理台：登录门 + 分区导航 + 各业务页。 */

import { useEffect, useState } from 'react';
import { NavLink, Navigate, Route, Routes } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { Button, Field, TextInput } from '@/ui/primitives/controls';
import { useAdminStore } from './adminStore';
import { AdminUsers } from './AdminUsers';
import { AdminReports } from './AdminReports';
import { AdminMessages } from './AdminMessages';
import { AdminFlags } from './AdminFlags';
import { AdminAudit } from './AdminAudit';
import { AdminBroadcast } from './AdminBroadcast';
import { AdminArchive } from './AdminArchive';
import { AdminOverview } from './AdminOverview';

const SECTIONS = [
  { to: '/admin', label: '概览', icon: 'metrics' as const, end: true },
  { to: '/admin/users', label: '用户', icon: 'group' as const },
  { to: '/admin/reports', label: '举报', icon: 'report' as const },
  { to: '/admin/messages', label: '消息', icon: 'chat' as const },
  { to: '/admin/flags', label: '开关', icon: 'settings' as const },
  { to: '/admin/audit', label: '审计', icon: 'audit' as const },
  { to: '/admin/broadcast', label: '广播', icon: 'broadcast' as const },
  { to: '/admin/archive', label: '归档', icon: 'archive' as const },
];

export function AdminApp() {
  const admin = useAdminStore();
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [totp, setTotp] = useState('');

  useEffect(() => {
    if (admin.token && !admin.identity) void admin.whoami();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [admin.token]);

  if (!admin.token) {
    return (
      <div className="uf-login">
        <div className="uf-login__card">
          <div className="uf-login__brand">
            <span className="uf-login__logo">UFOT</span>
            <span className="uf-faint">管理台</span>
          </div>
          <div className="uf-login__form">
            <Field label="管理员账号">
              {(id) => (
                <TextInput id={id} value={username} onChange={(e) => setUsername(e.target.value)} />
              )}
            </Field>
            <Field label="密码">
              {(id) => (
                <TextInput
                  id={id}
                  type="password"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                />
              )}
            </Field>
            <Field label="TOTP 验证码" hint="启用二步验证后必填">
              {(id) => (
                <TextInput
                  id={id}
                  value={totp}
                  inputMode="numeric"
                  onChange={(e) => setTotp(e.target.value)}
                />
              )}
            </Field>
            {admin.error ? (
              <div className="uf-toast uf-toast--error">
                <Icon name="warning" size="sm" />
                <span className="uf-grow">{admin.error}</span>
              </div>
            ) : null}
            <Button
              variant="primary"
              block
              loading={admin.busy}
              onClick={() => void admin.login(username, password, totp || undefined)}
            >
              登录
            </Button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="uf-admin">
      <aside className="uf-admin__nav" aria-label="管理导航">
        <div className="uf-admin__brand">UFOT 管理台</div>
        {SECTIONS.map((section) => (
          <NavLink
            key={section.to}
            to={section.to}
            end={section.end}
            className={({ isActive }) =>
              `uf-admin__link${isActive ? ' uf-admin__link--active' : ''}`
            }
          >
            <Icon name={section.icon} size="sm" />
            <span>{section.label}</span>
          </NavLink>
        ))}
        <div className="uf-grow" />
        <div className="uf-faint">当前：{admin.identity?.username ?? '—'}</div>
        <Button small icon="logout" onClick={() => void admin.logout()}>
          退出
        </Button>
      </aside>

      <main className="uf-admin__main">
        <Routes>
          <Route index element={<AdminOverview />} />
          <Route path="users" element={<AdminUsers />} />
          <Route path="reports" element={<AdminReports />} />
          <Route path="messages" element={<AdminMessages />} />
          <Route path="flags" element={<AdminFlags />} />
          <Route path="audit" element={<AdminAudit />} />
          <Route path="broadcast" element={<AdminBroadcast />} />
          <Route path="archive" element={<AdminArchive />} />
          <Route path="*" element={<Navigate to="/admin" replace />} />
        </Routes>
      </main>
    </div>
  );
}

/** 管理台分区内的页头。 */
export function AdminSection({
  title,
  actions,
}: {
  title: string;
  actions?: React.ReactNode;
}) {
  return (
    <header className="uf-topbar">
      <span className="uf-topbar__title">{title}</span>
      <div className="uf-grow" />
      {actions}
    </header>
  );
}
