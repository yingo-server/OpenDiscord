/**
 * 邮箱魔法链接：发送 → 轮询登录态（回跳/点击后自动进入）。
 */
import { useEffect, useMemo, useState } from 'react';
import { EmailAuthProvider, isValidEmail, normalizeEmail } from '../../../net/auth/providers/email';
import { AuthError, type AuthChallenge } from '../../../net/auth/provider';
import { getAppServices } from '../../services/app-services';
import { Button } from '../../components/Button';
import { Input } from '../../components/Input';
import { t } from '../../i18n';
import { authErrorText } from './auth-error';

export function EmailLogin() {
  const api = getAppServices().authApi;
  const provider = useMemo(() => new EmailAuthProvider(api), [api]);
  const [email, setEmail] = useState('');
  const [challenge, setChallenge] = useState<AuthChallenge | null>(null);
  const [seconds, setSeconds] = useState(0);
  const [sending, setSending] = useState(false);
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    if (seconds <= 0) return;
    const timer = setTimeout(() => setSeconds((value) => Math.max(0, value - 1)), 1000);
    return () => clearTimeout(timer);
  }, [seconds]);

  const verify = async () => {
    if (!challenge || checking) return;
    setChecking(true);
    setError('');
    try {
      const session = await provider.verify({
        email: normalizeEmail(email),
        challengeId: challenge.challengeId,
      });
      getAppServices().authenticate(session);
    } catch (caught) {
      if (caught instanceof AuthError && caught.code === 'PENDING') return;
      setError(authErrorText(caught));
    } finally {
      setChecking(false);
    }
  };

  // 发送后每 2s 轮询；成功后组件随会话切换卸载
  useEffect(() => {
    if (!challenge) return;
    const timer = setInterval(() => void verify(), 2000);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [challenge]);

  const sendLink = async () => {
    if (sending || seconds > 0) return;
    setError('');
    if (!isValidEmail(email)) {
      setError(t('auth.error.INVALID_EMAIL'));
      return;
    }
    setSending(true);
    try {
      const next = await provider.request({ email: normalizeEmail(email) });
      setChallenge(next);
      setSeconds(next.resendAfter || 60);
    } catch (caught) {
      setError(authErrorText(caught));
    } finally {
      setSending(false);
    }
  };

  return (
    <form
      className="space-y-4"
      onSubmit={(event) => {
        event.preventDefault();
        if (challenge) void verify();
        else void sendLink();
      }}
    >
      <Input
        label={t('auth.tab.email')}
        placeholder={t('auth.emailPlaceholder')}
        type="email"
        autoComplete="email"
        value={email}
        onChange={(value) => {
          setEmail(value);
          if (challenge) {
            setChallenge(null);
            setSeconds(0);
          }
        }}
        onClear={() => setEmail('')}
        disabled={sending}
      />

      {challenge ? (
        <div className="fluent-card space-y-3 bg-surface-2 p-3">
          <p className="text-body-sm text-ink">{t('auth.linkSent', { email: normalizeEmail(email) })}</p>
          <p className="text-caption text-ink-tertiary">{t('auth.linkSentHint')}</p>
          <div className="flex flex-wrap gap-2">
            <a href={`mailto:${normalizeEmail(email)}`} className="inline-flex">
              <Button variant="secondary" size="sm">
                {t('auth.openMail')}
              </Button>
            </a>
            <Button size="sm" onClick={() => void verify()} loading={checking}>
              {checking ? t('auth.polling') : t('auth.clicked')}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={seconds > 0}
              onClick={() => void sendLink()}
              className="nums"
            >
              {seconds > 0 ? t('auth.resendIn', { seconds }) : t('auth.resend')}
            </Button>
          </div>
        </div>
      ) : (
        <Button type="submit" block size="lg" loading={sending} disabled={!email}>
          {t('auth.sendLink')}
        </Button>
      )}

      {error ? (
        <p role="alert" className="fluent-error">
          {error}
        </p>
      ) : null}

      {import.meta.env.VITE_MOCK === '1' ? (
        <p className="text-center text-caption text-ink-tertiary">{t('auth.mockHint')}</p>
      ) : null}
    </form>
  );
}
