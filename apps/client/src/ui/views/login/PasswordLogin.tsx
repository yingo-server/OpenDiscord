/**
 * 密码登录：失败 5 次后出现图形验证码（后端 code=CAPTCHA_REQUIRED）。
 */
import { useMemo, useState } from 'react';
import { PasswordAuthProvider } from '../../../net/auth/providers/password';
import { AuthError } from '../../../net/auth/provider';
import { getAppServices } from '../../services/app-services';
import { Button } from '../../components/Button';
import { Input } from '../../components/Input';
import { t } from '../../i18n';
import { authErrorText } from './auth-error';

export function PasswordLogin() {
  const api = getAppServices().authApi;
  const provider = useMemo(() => new PasswordAuthProvider(api), [api]);
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');
  const [captcha, setCaptcha] = useState('');
  const [needCaptcha, setNeedCaptcha] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  const submit = async () => {
    if (submitting) return;
    setError('');
    setSubmitting(true);
    try {
      const session = await provider.request({
        account,
        password,
        captcha: needCaptcha ? captcha : undefined,
      });
      getAppServices().authenticate(session);
    } catch (caught) {
      if (caught instanceof AuthError && caught.code === 'CAPTCHA_REQUIRED') {
        setNeedCaptcha(true);
      }
      setError(authErrorText(caught));
      setSubmitting(false);
    }
  };

  return (
    <form
      className="space-y-4"
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <Input
        label={t('auth.tab.password')}
        placeholder={t('auth.accountPlaceholder')}
        value={account}
        autoComplete="username"
        onChange={setAccount}
        onClear={() => setAccount('')}
      />
      <Input
        label={t('auth.password')}
        placeholder={t('auth.passwordPlaceholder')}
        type="password"
        value={password}
        autoComplete="current-password"
        onChange={setPassword}
        disabled={submitting}
      />
      {needCaptcha ? (
        <Input
          label={t('auth.captcha')}
          placeholder={t('auth.captchaPlaceholder')}
          value={captcha}
          onChange={setCaptcha}
          autoComplete="off"
        />
      ) : null}

      {error ? (
        <p role="alert" className="fluent-error">
          {error}
        </p>
      ) : null}

      <Button type="submit" block size="lg" loading={submitting} disabled={!account || !password}>
        {submitting ? t('auth.loggingIn') : t('auth.login')}
      </Button>

      {import.meta.env.VITE_MOCK === '1' ? (
        <p className="text-center text-caption text-ink-tertiary">{t('auth.mockHint')}</p>
      ) : null}
    </form>
  );
}
