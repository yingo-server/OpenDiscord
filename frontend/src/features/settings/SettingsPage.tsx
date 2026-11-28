/** 我的：资料、改密、主题、推送、连接诊断、登出。 */

import { useState } from 'react';
import { Icon } from '@/ui/primitives/Icon';
import { Button, Field, TextInput } from '@/ui/primitives/controls';
import { useAuthStore } from '@/core/store/auth';
import { useUiStore } from '@/core/store/ui';
import { useConnStore } from '@/core/store/bridge';
import { env } from '@/core/config/env';
import { request } from '@/core/net/http';
import { requestUpdateProfile } from '@/core/store/bridge';

function base64UrlToUint8Array(base64: string): Uint8Array {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const normalized = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(normalized);
  const output = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i += 1) output[i] = raw.charCodeAt(i);
  return output;
}

export function SettingsPage() {
  const auth = useAuthStore();
  const theme = useUiStore((s) => s.theme);
  const setTheme = useUiStore((s) => s.setTheme);
  const pushToast = useUiStore((s) => s.pushToast);
  const conn = useConnStore();

  const [displayName, setDisplayName] = useState(auth.session?.profile?.display_name ?? '');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);

  const saveProfile = async () => {
    setBusy(true);
    try {
      await requestUpdateProfile(displayName.trim());
      auth.updateProfile({ display_name: displayName.trim() });
      pushToast('昵称已保存', 'info');
    } catch (error) {
      pushToast(error instanceof Error ? error.message : '保存失败，请重试', 'error');
    } finally {
      setBusy(false);
    }
  };

  const changePassword = async () => {
    if (password.length < 10) {
      pushToast('密码至少 10 个字符', 'warn');
      return;
    }
    setBusy(true);
    try {
      await auth.setPassword(password);
      setPassword('');
    } catch (error) {
      pushToast(error instanceof Error ? error.message : '修改失败', 'error');
    } finally {
      setBusy(false);
    }
  };

  const connLabel =
    conn.state === 'open'
      ? '已连接'
      : conn.state === 'connecting'
        ? '连接中'
        : conn.state === 'reconnecting'
          ? '重连中'
          : conn.state === 'failed'
            ? '连接失败'
            : '未连接';

  const enablePush = async () => {
    if (!env.vapidPublicKey) {
      pushToast('推送暂未开启，请联系管理员配置。', 'warn');
      return;
    }
    if (!('serviceWorker' in navigator) || !('PushManager' in window)) {
      pushToast('当前浏览器不支持 Web Push', 'warn');
      return;
    }
    try {
      const registration = await navigator.serviceWorker.register('/sw.js');
      const subscription = await registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: base64UrlToUint8Array(env.vapidPublicKey) as BufferSource,
      });
      await request('/api/push/subscribe', {
        method: 'POST',
        base: env.apiBase,
        body: { subscription: subscription.toJSON() },
      });
      pushToast('已开启推送通知', 'info');
    } catch (error) {
      pushToast(error instanceof Error ? error.message : '开启推送失败', 'error');
    }
  };

  return (
    <div className="uf-page">
      <header className="uf-topbar">
        <span className="uf-topbar__title">我的</span>
      </header>

      <div className="uf-page__body">
        <section className="uf-card">
          <h3 className="uf-card__title">账号</h3>
          <div className="uf-row" style={{ marginBottom: 'var(--sp-3)' }}>
            <Icon name="user" />
            <span className="uf-grow truncate">
              {auth.session?.profile?.display_name || '未设置昵称'}
            </span>
            <span className="uf-faint">ID {auth.session?.user_id ?? '—'}</span>
          </div>
          <div className="uf-faint">登录编号：{auth.session?.session_id ?? '—'}</div>
          <div className="uf-faint">{auth.session?.guest ? '游客登录' : '已注册账号'}</div>
        </section>

        <section className="uf-card">
          <h3 className="uf-card__title">昵称</h3>
          <Field label="显示昵称" hint="最多 32 个字符，保存后对方可见">
            {(id) => (
              <TextInput
                id={id}
                value={displayName}
                maxLength={32}
                onChange={(event) => setDisplayName(event.target.value)}
              />
            )}
          </Field>
          <Button onClick={() => void saveProfile()} loading={busy} style={{ marginTop: 'var(--sp-3)' }}>
            保存昵称
          </Button>
        </section>

        <section className="uf-card">
          <h3 className="uf-card__title">安全</h3>
          <Field label="新密码" hint="至少 10 个字符">
            {(id) => (
              <TextInput
                id={id}
                type="password"
                value={password}
                autoComplete="new-password"
                onChange={(event) => setPassword(event.target.value)}
              />
            )}
          </Field>
          <Button onClick={() => void changePassword()} loading={busy} style={{ marginTop: 'var(--sp-3)' }}>
            修改密码
          </Button>
        </section>

        <section className="uf-card">
          <h3 className="uf-card__title">外观</h3>
          <div className="uf-row">
            <Button
              icon="moon"
              variant={theme === 'dark' ? 'primary' : 'default'}
              onClick={() => setTheme('dark')}
            >
              暗色
            </Button>
            <Button
              icon="sun"
              variant={theme === 'light' ? 'primary' : 'default'}
              onClick={() => setTheme('light')}
            >
              亮色
            </Button>
            <Button
              icon="refresh"
              variant={theme === 'system' ? 'primary' : 'default'}
              onClick={() => setTheme('system')}
            >
              跟随系统
            </Button>
          </div>
        </section>

        <section className="uf-card">
          <h3 className="uf-card__title">通知</h3>
          <div className="uf-row">
            <Icon name="bell" />
            <span className="uf-grow">Web Push 通知</span>
            <Button onClick={() => void enablePush()}>开启</Button>
          </div>
          <div className="uf-faint" style={{ marginTop: 'var(--sp-2)' }}>
            如无法开启通知，请稍后再试或联系管理员。
          </div>
        </section>

        <section className="uf-card">
          <h3 className="uf-card__title">连接</h3>
          <div className="uf-row">
            <Icon name="online" />
            <span className="uf-grow">连接状态</span>
            <span className="uf-faint">{connLabel}{conn.latencyMs ? ` · ${conn.latencyMs} 毫秒` : ''}</span>
          </div>
        </section>

        <section className="uf-card">
          <Button variant="danger" icon="logout" onClick={() => void auth.logout()}>
            退出登录
          </Button>
        </section>
      </div>
    </div>
  );
}
