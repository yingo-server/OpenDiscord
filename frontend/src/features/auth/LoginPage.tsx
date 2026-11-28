/** 登录页：游客 / 密码两种方式（其它方式由后端 methods 探测决定是否出现，当前生产不支持）。 */

import { useEffect, useState } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import { Button, Field, Tabs, TextInput } from '@/ui/primitives/controls';
import { useAuthStore } from '@/core/store/auth';

type Method = 'guest' | 'password';

export function LoginPage() {
  const auth = useAuthStore();
  const location = useLocation();
  const [method, setMethod] = useState<Method>('guest');
  const [nickname, setNickname] = useState('');
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');

  // methods 探测仍保留：只决定 guest / password 两个 tab 哪个可用。
  useEffect(() => {
    void auth.loadMethods();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const available: Method[] = [
    ...(auth.methods?.guest === false ? [] : (['guest'] as Method[])),
    ...(auth.methods?.password === false ? [] : (['password'] as Method[])),
  ];
  if (!available.length) available.push(auth.methods?.password ? 'password' : 'guest');

  useEffect(() => {
    if (!available.includes(method)) setMethod(available[0] ?? 'guest');
  }, [available, method]);

  if (auth.status === 'authenticated') {
    const target = (location.state as { from?: string } | null)?.from ?? '/chats';
    return <Navigate to={target} replace />;
  }

  const submit = async () => {
    if (method === 'guest') {
      await auth.login({ method: 'guest', nickname: nickname.trim() || undefined });
    } else {
      await auth.login({ method: 'password', account, password });
    }
  };

  return (
    <div className="uf-login">
      <div className="uf-login__card">
        <div className="uf-login__brand">
          <span className="uf-login__logo">UFOT</span>
          <span className="uf-faint">Unfettered Freedom Of Thought</span>
        </div>

        {auth.methods ? (
          <Tabs
            value={method}
            options={available.map((m) => ({
              value: m,
              label: m === 'guest' ? '游客' : '密码',
            }))}
            onChange={setMethod}
            ariaLabel="登录方式"
          />
        ) : (
          <div className="uf-faint">正在获取登录方式…</div>
        )}

        <div className="uf-login__form">
          {method === 'guest' ? (
            <Field label="昵称（可选）" hint="不填则由服务端生成游客昵称">
              {(id) => (
                <TextInput
                  id={id}
                  value={nickname}
                  maxLength={32}
                  placeholder="游客"
                  onChange={(event) => setNickname(event.target.value)}
                />
              )}
            </Field>
          ) : (
            <>
              <Field label="账号">
                {(id) => (
                  <TextInput
                    id={id}
                    value={account}
                    autoComplete="username"
                    placeholder="用户名"
                    onChange={(event) => setAccount(event.target.value)}
                  />
                )}
              </Field>
              <Field label="密码">
                {(id) => (
                  <TextInput
                    id={id}
                    type="password"
                    value={password}
                    autoComplete="current-password"
                    onChange={(event) => setPassword(event.target.value)}
                  />
                )}
              </Field>
            </>
          )}

          {auth.error ? (
            <div className="uf-toast uf-toast--error">{auth.error}</div>
          ) : null}
          {auth.notice ? <div className="uf-toast uf-toast--info">{auth.notice}</div> : null}
          {auth.methodsError ? (
            <div className="uf-toast uf-toast--warn">{auth.methodsError}</div>
          ) : null}

          <Button variant="primary" block loading={auth.busy} onClick={() => void submit()}>
            登录
          </Button>
        </div>
      </div>
    </div>
  );
}
