/**
 * 游客进入：受限会话（文字可用；媒体/建群引导升级）。
 */
import { useMemo, useState } from 'react';
import { GuestAuthProvider } from '../../../net/auth/providers/guest';
import { getAppServices } from '../../services/app-services';
import { Button } from '../../components/Button';
import { Input } from '../../components/Input';
import { t } from '../../i18n';
import { authErrorText } from './auth-error';

export function GuestEntry() {
  const api = getAppServices().authApi;
  const provider = useMemo(() => new GuestAuthProvider(api), [api]);
  const [nickname, setNickname] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  const enter = async () => {
    if (submitting) return;
    setSubmitting(true);
    setError('');
    try {
      const session = await provider.request({ nickname });
      getAppServices().authenticate(session);
    } catch (caught) {
      setError(authErrorText(caught));
      setSubmitting(false);
    }
  };

  return (
    <div className="space-y-4">
      <div className="fluent-card bg-surface-2 p-3">
        <p className="text-body-sm text-ink">{t('auth.guestTitle')}</p>
        <p className="mt-1 text-caption text-ink-tertiary">{t('auth.guestHint')}</p>
      </div>
      <Input
        label={t('auth.nicknamePlaceholder')}
        placeholder={t('auth.nicknamePlaceholder')}
        value={nickname}
        maxLength={20}
        onChange={setNickname}
        onClear={() => setNickname('')}
      />
      {error ? (
        <p role="alert" className="fluent-error">
          {error}
        </p>
      ) : null}
      <Button block size="lg" loading={submitting} onClick={() => void enter()}>
        {t('auth.guestEnter')}
      </Button>
    </div>
  );
}
