/**
 * 手机验证码登录：发码 60s 倒计时 → 6 位 OTP 自动提交。
 */
import { useEffect, useMemo, useState } from 'react';
import { PhoneAuthProvider, isValidPhone, normalizePhone } from '../../../net/auth/providers/phone';
import type { AuthChallenge } from '../../../net/auth/provider';
import { getAppServices } from '../../services/app-services';
import { Button } from '../../components/Button';
import { Input } from '../../components/Input';
import { t } from '../../i18n';
import { authErrorText } from './auth-error';

export function PhoneLogin() {
  const api = getAppServices().authApi;
  const provider = useMemo(() => new PhoneAuthProvider(api), [api]);
  const [phone, setPhone] = useState('');
  const [code, setCode] = useState('');
  const [challenge, setChallenge] = useState<AuthChallenge | null>(null);
  const [seconds, setSeconds] = useState(0);
  const [sending, setSending] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  useEffect(() => {
    if (seconds <= 0) return;
    const timer = setTimeout(() => setSeconds((value) => Math.max(0, value - 1)), 1000);
    return () => clearTimeout(timer);
  }, [seconds]);

  const sendCode = async () => {
    if (sending || seconds > 0) return;
    setError('');
    if (!isValidPhone(phone)) {
      setError(t('auth.error.INVALID_PHONE'));
      return;
    }
    setSending(true);
    try {
      const next = await provider.request({ phone: normalizePhone(phone) });
      setChallenge(next);
      setSeconds(next.resendAfter || 60);
      setCode('');
    } catch (caught) {
      setError(authErrorText(caught));
    } finally {
      setSending(false);
    }
  };

  const verify = async (value: string) => {
    if (!challenge || submitting || value.length !== 6) return;
    setSubmitting(true);
    setError('');
    try {
      const session = await provider.verify({
        phone: normalizePhone(phone),
        code: value,
        challengeId: challenge.challengeId,
      });
      getAppServices().authenticate(session);
    } catch (caught) {
      setError(authErrorText(caught));
      setSubmitting(false);
    }
  };

  return (
    <form
      className="space-y-4"
      onSubmit={(event) => {
        event.preventDefault();
        void verify(code);
      }}
    >
      <Input
        label={t('auth.phone')}
        placeholder={t('auth.phonePlaceholder')}
        value={phone}
        inputMode="tel"
        autoComplete="tel"
        maxLength={20}
        onChange={(value) => {
          setPhone(value);
          if (challenge) {
            setChallenge(null);
            setSeconds(0);
            setCode('');
          }
        }}
        onClear={() => setPhone('')}
        disabled={submitting}
      />

      {challenge ? (
        <>
          <div className="flex items-end gap-2">
            <div className="flex-1">
              <Input
                label={t('auth.code')}
                placeholder={t('auth.codePlaceholder')}
                value={code}
                inputMode="numeric"
                autoComplete="one-time-code"
                maxLength={6}
                className="nums tracking-[0.3em]"
                onChange={(value) => {
                  const digits = value.replace(/\D/g, '').slice(0, 6);
                  setCode(digits);
                  if (digits.length === 6) void verify(digits);
                }}
                disabled={submitting}
              />
            </div>
            <Button
              variant="secondary"
              onClick={() => void sendCode()}
              disabled={seconds > 0}
              loading={sending}
              className="nums mb-0.5"
            >
              {seconds > 0 ? t('auth.resendIn', { seconds }) : t('auth.resend')}
            </Button>
          </div>
          {challenge.hint ? <p className="text-caption text-ink-tertiary">{challenge.hint}</p> : null}
        </>
      ) : (
        <Button
          variant="secondary"
          block
          onClick={() => void sendCode()}
          loading={sending}
          disabled={!phone}
        >
          {t('auth.getCode')}
        </Button>
      )}

      {error ? (
        <p role="alert" className="fluent-error">
          {error}
        </p>
      ) : null}

      <Button type="submit" block size="lg" loading={submitting} disabled={!challenge || code.length !== 6}>
        {t('auth.login')}
      </Button>

      {import.meta.env.VITE_MOCK === '1' ? (
        <p className="text-center text-caption text-ink-tertiary">{t('auth.mockHint')}</p>
      ) : null}
    </form>
  );
}
