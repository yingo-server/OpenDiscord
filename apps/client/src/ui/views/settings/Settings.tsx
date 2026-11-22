/**
 * 设置（§7.3 字号 / §1.2 主题）：外观、账号、功能开关、连接、数据与退出。
 * 破坏性操作一律二次确认。
 */
import { useState, type ReactNode } from 'react';
import { useSessionStore } from '../../../core/stores/session-store';
import { useConnStore } from '../../../core/stores/conn-store';
import { useMessageStore } from '../../../core/stores/message-store';
import { useConvStore } from '../../../core/stores/conv-store';
import { getAppServices } from '../../services/app-services';
import { Avatar } from '../../components/Avatar';
import { Button } from '../../components/Button';
import { IconButton } from '../../components/IconButton';
import { BackIcon, LogoutIcon, MonitorIcon, MoonIcon, SunIcon, TrashIcon } from '../../components/icons';
import { t } from '../../i18n';
import { navigate } from '../../router';
import { applyFontScale, applyTheme, getFontScale, getThemePref, type FontScale, type ThemePref } from '../../theme';
import { toast } from '../../components/Toast';

type ConfirmKind = 'logout' | 'clear' | null;

const THEMES: Array<{ id: ThemePref; labelKey: 'settings.theme.system' | 'settings.theme.light' | 'settings.theme.dark'; icon: ReactNode }> = [
  { id: 'system', labelKey: 'settings.theme.system', icon: <MonitorIcon size={18} /> },
  { id: 'light', labelKey: 'settings.theme.light', icon: <SunIcon size={18} /> },
  { id: 'dark', labelKey: 'settings.theme.dark', icon: <MoonIcon size={18} /> },
];

const SCALES: Array<{ id: FontScale; labelKey: 'settings.font.standard' | 'settings.font.large' | 'settings.font.xlarge' }> = [
  { id: 'standard', labelKey: 'settings.font.standard' },
  { id: 'large', labelKey: 'settings.font.large' },
  { id: 'xlarge', labelKey: 'settings.font.xlarge' },
];

export function Settings() {
  const session = useSessionStore();
  const conn = useConnStore();
  const [theme, setTheme] = useState<ThemePref>(() => getThemePref());
  const [scale, setScale] = useState<FontScale>(() => getFontScale());
  const [confirm, setConfirm] = useState<ConfirmKind>(null);

  const changeTheme = (next: ThemePref) => {
    setTheme(next);
    applyTheme(next);
  };

  const changeScale = (next: FontScale) => {
    setScale(next);
    applyFontScale(next);
  };

  const clearCache = async () => {
    const services = getAppServices();
    await services.cache?.clear();
    useMessageStore.getState().clear();
    useConvStore.getState().clear();
    setConfirm(null);
    toast.success(t('settings.cleared'));
  };

  const stateLabel =
    conn.state === 'online'
      ? t('network.online')
      : conn.state === 'offline'
        ? t('network.offline')
        : t('network.connecting');

  return (
    <section className="scroll-thin h-full overflow-y-auto bg-page">
      <header className="pt-safe sticky top-0 z-sticky flex h-14 items-center gap-2 border-b border-line bg-surface px-2 tablet:px-4">
        <IconButton
          label={t('common.back')}
          onClick={() => navigate('/chats')}
          icon={<BackIcon size={22} />}
        />
        <h1 className="fluent-title">{t('settings.title')}</h1>
      </header>

      <div className="mx-auto w-full max-w-[720px] space-y-4 p-4">
        {/* 账号 */}
        <div className="fluent-card flex items-center gap-3 p-4">
          <Avatar name={session.displayName} size="lg" userId={session.userId} presence="online" />
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <p className="truncate text-body font-semibold text-ink">{session.displayName}</p>
              {session.status === 'guest' ? (
                <span className="rounded-full bg-warning-100 px-2 py-0.5 text-caption text-warning-700">
                  {t('settings.guest')}
                </span>
              ) : null}
            </div>
            <p className="nums mt-0.5 text-body-sm text-ink-tertiary">
              {t('settings.userId')}: {session.userId || '-'}
            </p>
          </div>
          {session.status === 'guest' ? (
            <Button size="sm" variant="secondary" onClick={() => toast.info(t('toast.comingSoon'))}>
              {t('settings.upgrade')}
            </Button>
          ) : null}
        </div>

        {/* 外观 */}
        <Card title={t('settings.appearance')}>
          <Row label={t('settings.theme')}>
            <Segmented>
              {THEMES.map((item) => (
                <Segment key={item.id} active={theme === item.id} onClick={() => changeTheme(item.id)}>
                  <span className="flex items-center gap-1.5">
                    {item.icon}
                    <span className="hidden tablet:inline">{t(item.labelKey)}</span>
                  </span>
                </Segment>
              ))}
            </Segmented>
          </Row>
          <Row label={t('settings.fontScale')}>
            <Segmented>
              {SCALES.map((item) => (
                <Segment key={item.id} active={scale === item.id} onClick={() => changeScale(item.id)}>
                  {t(item.labelKey)}
                </Segment>
              ))}
            </Segmented>
          </Row>
        </Card>

        {/* 连接 */}
        <Card title={t('settings.connection')}>
          <Row label={t('settings.connectionState')}>
            <span className={conn.state === 'online' ? 'text-success-500' : 'text-ink-secondary'}>
              {stateLabel}
            </span>
          </Row>
          <Row label={t('settings.latency')}>
            <span className="nums">{conn.latencyMs ? t('network.latency', { ms: conn.latencyMs }) : '-'}</span>
          </Row>
        </Card>

        {/* 功能开关 */}
        <Card title={t('settings.flags')}>
          {Object.keys(session.flags).length === 0 ? (
            <p className="text-body-sm text-ink-tertiary">-</p>
          ) : (
            <div className="space-y-2">
              {Object.entries(session.flags).map(([key, value]) => (
                <div key={key} className="flex items-center justify-between gap-3 text-body-sm">
                  <span className="truncate font-nums text-ink-secondary">{key}</span>
                  <span className="nums rounded-sm bg-surface-2 px-2 py-0.5 text-caption text-ink">
                    {value}
                  </span>
                </div>
              ))}
            </div>
          )}
        </Card>

        {/* 数据 */}
        <Card title={t('settings.data')}>
          <button
            type="button"
            onClick={() => setConfirm('clear')}
            className="flex min-h-11 w-full items-center gap-2 rounded-md px-2 text-start text-body text-danger-500 transition-colors duration-micro hover:bg-danger-50"
          >
            <TrashIcon size={18} />
            {t('settings.clearCache')}
          </button>
        </Card>

        <button
          type="button"
          onClick={() => setConfirm('logout')}
          className="flex min-h-11 w-full items-center justify-center gap-2 rounded-md border border-line-input bg-surface text-body text-danger-500 transition-colors duration-micro hover:bg-danger-50"
        >
          <LogoutIcon size={18} />
          {t('settings.logout')}
        </button>

        <div className="pb-6 text-center text-caption text-ink-tertiary">
          <p>
            {t('settings.version')} 0.1.0 · {t('settings.about')}
          </p>
          <p className="mt-1">{t('settings.aboutText')}</p>
        </div>
      </div>

      {confirm ? (
        <ConfirmDialog
          title={confirm === 'logout' ? t('settings.logout') : t('settings.clearCache')}
          message={confirm === 'logout' ? t('settings.logoutConfirm') : t('settings.clearConfirm')}
          confirmLabel={
            confirm === 'logout' ? t('settings.logoutConfirmAction') : t('settings.clearConfirmAction')
          }
          onCancel={() => setConfirm(null)}
          onConfirm={() => {
            if (confirm === 'logout') {
              setConfirm(null);
              getAppServices().logout();
            } else {
              void clearCache();
            }
          }}
        />
      ) : null}
    </section>
  );
}

function Card({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="fluent-card p-4">
      <h2 className="mb-3 text-body-sm font-semibold uppercase tracking-wide text-ink-secondary">{title}</h2>
      <div className="space-y-3">{children}</div>
    </div>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-2">
      <span className="text-body text-ink">{label}</span>
      {children}
    </div>
  );
}

function Segmented({ children }: { children: ReactNode }) {
  return <div className="flex rounded-md border border-line bg-surface-2 p-0.5">{children}</div>;
}

function Segment({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={[
        'inline-flex h-8 items-center justify-center rounded-sm px-3 text-body-sm transition-colors duration-micro',
        active ? 'fluent-focus bg-surface text-ink shadow-sm' : 'text-ink-tertiary hover:text-ink',
      ].join(' ')}
    >
      {children}
    </button>
  );
}

function ConfirmDialog({
  title,
  message,
  confirmLabel,
  onCancel,
  onConfirm,
}: {
  title: string;
  message: string;
  confirmLabel: string;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  return (
    <div className="fixed inset-0 z-modal flex items-center justify-center bg-scrim/40 p-4 backdrop-blur-[2px]" onClick={onCancel}>
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="confirm-title"
        className="fluent-flyout w-full max-w-[400px] animate-scale-in p-6"
        onClick={(event) => event.stopPropagation()}
      >
        <h2 id="confirm-title" className="fluent-title">
          {title}
        </h2>
        <p className="mt-2 text-body-sm text-ink-secondary">{message}</p>
        <div className="mt-6 flex justify-end gap-2">
          <Button variant="secondary" onClick={onCancel}>
            {t('common.cancel')}
          </Button>
          <Button variant="danger" onClick={onConfirm}>
            {confirmLabel}
          </Button>
        </div>
      </div>
    </div>
  );
}
