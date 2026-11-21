/**
 * 封禁页（§6.4）：全屏路由替换，不可返回；展示原因与解封时间。
 */
import { useSessionStore } from '../../../core/stores/session-store';
import { getAppServices } from '../../services/app-services';
import { Button } from '../../components/Button';
import { AlertIcon } from '../../components/icons';
import { t } from '../../i18n';
import { formatDateTime } from '../../utils/format';

export function Banned() {
  const ban = useSessionStore((state) => state.ban);

  return (
    <div className="flex min-h-[100dvh] flex-col items-center justify-center bg-page px-6 py-12 text-center">
      <div className="mb-5 flex h-32 w-32 items-center justify-center rounded-full bg-danger-50 text-danger-500">
        <AlertIcon size={56} />
      </div>
      <h1 className="text-display text-ink">{t('banned.title')}</h1>
      <p className="mt-3 max-w-[420px] text-body text-ink-secondary">
        {t('banned.reason', { reason: ban?.reason || '-' })}
      </p>
      <p className="mt-1 max-w-[420px] text-body-sm text-ink-tertiary">
        {ban?.until ? t('banned.until', { time: formatDateTime(ban.until) }) : t('banned.permanent')}
      </p>
      <p className="mt-3 text-body-sm text-ink-tertiary">{t('banned.hint')}</p>

      <div className="mt-8 flex flex-wrap items-center justify-center gap-3">
        <a href="mailto:support@ufot.example" className="inline-flex">
          <Button>{t('banned.contact')}</Button>
        </a>
        <Button variant="secondary" onClick={() => getAppServices().logout()}>
          {t('banned.logout')}
        </Button>
      </div>
    </div>
  );
}
