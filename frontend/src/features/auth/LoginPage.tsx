/** 登录页：按后端 /api/auth/methods 动态渲染可用登录方式。 */

import { useEffect, useMemo, useState } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { Button, Field, Tabs, TextInput } from '@/ui/primitives/controls';
import { useAuthStore } from '@/core/store/auth';
import { requestEmailLink, requestPhoneCode } from '@/core/domain/auth';

type Method = 'guest' | 'password' | 'phone' | 'email';

export function LoginPage() {
  const auth = useAuthStore();
  const location = useLocation();
  const [method, setMethod] = useState<Method>('guest');
  const [nickname, setNickname] = useState('');
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');
  const [phone, setPhone] = useState('');
  const [code, setCode] = useState('');
  const [email, setEmail] = useState('');
  const [challengeId, setChallengeId] = useState<string | undefined>();
  const [resendIn, setResendIn] = useState(0);
  const [sending, setSending] = useState(false);

  useEffect(() => {
    void auth.loadMethods();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 倒计时（重发间隔由后端告知）
  useEffect(() => {
    if (resendIn <= 0) return;
    const timer = setTimeout(() => setResendIn(resendIn - 1), 1000);
    return () => clearTimeout(timer);
  }, [resendIn]);

  const methods = auth.methods;
  const available = useMemo<Method[]>(() => {
    if (!methods) return ['guest'];
    const list: Method[] = [];
    if (methods.guest) list.push('guest');
    if (methods.password) list.push('password');
    if (methods.phone) list.push('phone');
    if (methods.email) list.push('email');
    return list.length ? list : ['guest'];
  }, [methods]);

  useEffect(() => {
    if (!available.includes(method)) setMethod(available[0] ?? 'guest');
  }, [available, method]);

  if (auth.status === 'authenticated') {
    const target = (location.state as { from?: string } | null)?.from ?? '/chats';
    return <Navigate to={target} replace />;
  }

  const submit = async () => {
    switch (method) {
      case 'guest':
        await auth.login({ method: 'guest', nickname: nickname.trim() || undefined });
        break;
      case 'password':
        await auth.login({ method: 'password', account, password });
        break;
      case 'phone':
        await auth.login({ method: 'phone', phone, code, challengeId });
        break;
      case 'email':
        await auth.login({ method: 'email', email, challengeId });
        break;
    }
  };

  const sendCode = async () => {
    setSending(true);
    try {
      if (method === 'phone') {
        const challenge = await requestPhoneCode(phone);
        setChallengeId(challenge.challenge_id);
        setResendIn(challenge.resend_after ?? 60);
      } else if (method === 'email') {
        const challenge = await requestEmailLink(email);
        setChallengeId(challenge.challenge_id);
        setResendIn(challenge.resend_after ?? 60);
      }
    } catch (error) {
      auth.setNotice(error instanceof Error ? error.message : '发送失败');
    } finally {
      setSending(false);
    }
  };

  const tabs = available.map((m) => ({
    value: m,
    label:
      m === 'guest' ? '游客' : m === 'password' ? '密码' : m === 'phone' ? '手机' : '邮箱',
  }));

  return (
    <div className="uf-login">
      <div className="uf-login__card">
        <div className="uf-login__brand">
          <span className="uf-login__logo">UFOT</span>
          <span className="uf-faint">Unfettered Freedom Of Thought</span>
        </div>

        {methods ? (
          <Tabs value={method} options={tabs} onChange={setMethod} ariaLabel="登录方式" />
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
          ) : null}

          {method === 'password' ? (
            <>
              <Field label="账号">
                {(id) => (
                  <TextInput
                    id={id}
                    value={account}
                    autoComplete="username"
                    placeholder="手机号 / 邮箱 / 用户名"
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
          ) : null}

          {method === 'phone' ? (
            <>
              <Field label="手机号">
                {(id) => (
                  <TextInput
                    id={id}
                    value={phone}
                    inputMode="tel"
                    placeholder="+8613800138000"
                    onChange={(event) => setPhone(event.target.value)}
                  />
                )}
              </Field>
              <Field label="验证码">
                {(id) => (
                  <div className="uf-row">
                    <TextInput
                      id={id}
                      value={code}
                      inputMode="numeric"
                      onChange={(event) => setCode(event.target.value)}
                    />
                    <Button
                      onClick={() => void sendCode()}
                      disabled={sending || resendIn > 0 || !phone}
                    >
                      {resendIn > 0 ? `${resendIn}s` : '发送'}
                    </Button>
                  </div>
                )}
              </Field>
            </>
          ) : null}

          {method === 'email' ? (
            <>
              <Field label="邮箱" hint="服务端会发送魔法链接，点击后回到本页自动完成登录">
                {(id) => (
                  <TextInput
                    id={id}
                    value={email}
                    type="email"
                    onChange={(event) => setEmail(event.target.value)}
                  />
                )}
              </Field>
              <Button onClick={() => void sendCode()} disabled={sending || resendIn > 0 || !email}>
                {resendIn > 0 ? `重新发送（${resendIn}s）` : '发送登录链接'}
              </Button>
            </>
          ) : null}

          {auth.error ? (
            <div className="uf-toast uf-toast--error">
              <Icon name="warning" size="sm" />
              <span className="uf-grow break-anywhere">{auth.error}</span>
            </div>
          ) : null}
          {auth.notice ? (
            <div className="uf-toast uf-toast--info">
              <Icon name="info" size="sm" />
              <span className="uf-grow break-anywhere">{auth.notice}</span>
            </div>
          ) : null}
          {auth.methodsError ? (
            <div className="uf-toast uf-toast--warn">
              <Icon name="warning" size="sm" />
              <span className="uf-grow break-anywhere">{auth.methodsError}</span>
            </div>
          ) : null}

          <Button variant="primary" block loading={auth.busy} onClick={() => void submit()}>
            登录
          </Button>
        </div>
      </div>
    </div>
  );
}
