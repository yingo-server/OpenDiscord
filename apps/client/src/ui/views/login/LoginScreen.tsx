/**
 * 登录入口：四种 Provider 同屏切换（手机 / 邮箱 / 密码 / 游客）。
 * 各 Provider 只负责自己的表单，成功后统一走 services.authenticate。
 */
import { useState, type ComponentType } from 'react';
import { Tab, TabList } from '@fluentui/react-components';
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
          <TabList
            aria-label={t('auth.title')}
            className="mb-5 border-b border-line"
            selectedValue={tab}
            onTabSelect={(_, data) => setTab(data.value as TabId)}
          >
            {TABS.map(({ id, labelKey }) => (
              <Tab key={id} value={id} className="flex-1 whitespace-nowrap">
                {t(labelKey)}
              </Tab>
            ))}
          </TabList>

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
