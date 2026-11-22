/**
 * 登录入口：四种 Provider 同屏切换（手机 / 邮箱 / 密码 / 游客）。
 * 各 Provider 只负责自己的表单，成功后统一走 services.authenticate。
 */
import { useState, type ComponentType } from 'react';
import { t } from '../../i18n';
import { PhoneLogin } from './PhoneLogin';
import { EmailLogin } from './EmailLogin';
import { PasswordLogin } from './PasswordLogin';
import { GuestEntry } from './GuestEntry';

type TabId = 'phone' | 'email' | 'password' | 'guest';

const TABS: Array<{ id: TabId; labelKey: 'auth.tab.phone' | 'auth.tab.email' | 'auth.tab.password' | 'auth.tab.guest'; Component: ComponentType }> = [
  { id: 'phone', labelKey: 'auth.tab.phone', Component: PhoneLogin },
  { id: 'email', labelKey: 'auth.tab.email', Component: EmailLogin },
  { id: 'password', labelKey: 'auth.tab.password', Component: PasswordLogin },
  { id: 'guest', labelKey: 'auth.tab.guest', Component: GuestEntry },
];

export function LoginScreen() {
  const [tab, setTab] = useState<TabId>('phone');
  const Active = TABS.find((item) => item.id === tab)?.Component ?? PhoneLogin;

  return (
    <div className="flex min-h-[100dvh] flex-col items-center justify-center overflow-y-auto bg-page px-4 py-8">
      <div className="w-full max-w-[400px]">
        <div className="mb-8 flex flex-col items-center text-center">
          <div className="mb-3 flex h-12 w-12 items-center justify-center rounded-xl bg-accent text-accent-fg shadow-md" aria-hidden="true">
            <svg width="26" height="26" viewBox="0 0 24 24" fill="none" aria-hidden="true"><path d="M12 2.6 21.8 20H2.2L12 2.6Z" stroke="currentColor" strokeWidth="1.6" strokeLinejoin="round"/><path d="M12 9.4 17.4 19H6.6L12 9.4Z" fill="currentColor" fillOpacity="0.4"/></svg>
          </div>
          <h1 className="fluent-title">{t('auth.title')}</h1>
          <p className="mt-1 text-body-sm text-ink-tertiary">{t('auth.subtitle')}</p>
        </div>

        <div className="fluent-card p-4 tablet:p-6">
          <div role="tablist" aria-label={t('auth.title')} className="mb-5 flex border-b border-line">
            {TABS.map(({ id, labelKey }) => {
              const active = id === tab;
              return (
                <button
                  key={id}
                  role="tab"
                  aria-selected={active}
                  type="button"
                  onClick={() => setTab(id)}
                  className={[
                    'relative flex-1 whitespace-nowrap px-1 py-2.5 text-body-sm transition-colors duration-micro',
                    active ? 'font-semibold text-accent' : 'text-ink-secondary hover:text-ink',
                  ].join(' ')}
                >
                  {t(labelKey)}
                  {active ? (
                    <span className="absolute inset-x-2 -bottom-px h-0.5 rounded-full bg-accent" aria-hidden="true" />
                  ) : null}
                </button>
              );
            })}
          </div>

          <div role="tabpanel">
            <Active />
          </div>
        </div>

        <p className="mt-4 text-center text-caption text-ink-tertiary">{t('auth.terms')}</p>
        <p className="mt-1 text-center text-caption text-ink-tertiary/70">{t('app.tagline')}</p>
      </div>
    </div>
  );
}
