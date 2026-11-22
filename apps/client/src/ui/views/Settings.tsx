import { useEffect, useState, type ReactNode } from 'react';
import { getSession } from '../../state/session';
import { getAppServices } from '../../app/services';
import { currentTheme, applyTheme, clearLocalData, type ThemeMode } from '../../platform/theme';
import { subscribePush, pushAvailable, ApiError } from '../../net';
import { t } from '../i18n';
import { toast } from '../components/Toast';
import { CallSettingsRow } from '../components/CallOverlay';

export function Settings({ onBack }: { onBack: () => void }): ReactNode {
  const services = getAppServices();
  const session = getSession();
  const [name, setName] = useState(session?.profile.display_name || '');
  const [password, setPassword] = useState('');
  const [theme, setTheme] = useState<ThemeMode>(currentTheme());
  const [pushOn, setPushOn] = useState(false);
  useEffect(() => { setName(session?.profile.display_name || ''); }, [session]);

  async function saveProfile(): Promise<void> {
    const trimmed = name.trim();
    if (!trimmed) return;
    const previous = session?.profile.display_name || '';
    if (session) services.setSession({ ...session, profile: { ...session.profile, display_name: trimmed } });
    try {
      await services.updateProfile({ display_name: trimmed });
      toast(t('settings.saved'));
    } catch {
      if (session) services.setSession({ ...session, profile: { ...session.profile, display_name: previous } });
      setName(previous);
      toast(t('settings.save.failed'));
    }
  }

  function savePassword(): void {
    if (password.length < 8) { toast(t('settings.password.hint')); return; }
    void services.auth.setPassword(password)
      .then(() => { setPassword(''); toast(t('settings.saved')); })
      .catch(() => toast(t('error.generic')));
  }

  async function enablePush(): Promise<void> {
    try {
      await subscribePush(services.http);
      setPushOn(true);
      toast(t('push.enabled'));
    } catch (error) {
      toast(error instanceof ApiError ? error.message : t('push.failed'));
    }
  }

  async function logout(): Promise<void> {
    await services.logout();
    onBack();
  }

  return (
    <main className="settings-page">
      <div className="pane-head" style={{ padding: 0, marginBottom: 16 }}>
        <button className="icon-btn" type="button" onClick={onBack} aria-label={t('common.close')}>←</button>
        <h1>{t('settings.title')}</h1>
      </div>
      <section className="settings-section">
        <h2>{t('settings.account')}</h2>
        <div className="settings-card">
          <div className="settings-row">
            <span className="label">{t('settings.display.name')}</span>
            <input className="input" style={{ maxWidth: 200 }} value={name} onChange={(event) => setName(event.target.value)} />
            <button className="btn small" type="button" onClick={() => void saveProfile()}>{t('settings.save')}</button>
          </div>
          <div className="settings-row">
            <span className="label">{t('settings.password')}<small>{t('settings.password.hint')}</small></span>
            <input className="input" type="password" style={{ maxWidth: 200 }} value={password} onChange={(event) => setPassword(event.target.value)} placeholder="至少 8 位密码" />
            <button className="btn small" type="button" onClick={savePassword}>{t('settings.save')}</button>
          </div>
        </div>
      </section>
      <section className="settings-section">
        <h2>{t('settings.theme')}</h2>
        <div className="settings-card">
          <div className="settings-row">
            <span className="label">{t('settings.theme')}</span>
            <div className="segmented">
              {(['light', 'dark', 'system'] as const).map((mode) => (
                <button key={mode} type="button" className={theme === mode ? 'active' : ''} onClick={() => { setTheme(mode); applyTheme(mode); }}>
                  {t(`settings.theme.${mode}`)}
                </button>
              ))}
            </div>
          </div>
          <CallSettingsRow />
          <div className="settings-row">
            <span className="label">{t('push.enable')}<small>{pushOn ? t('push.enabled') : t('push.unavailable')}</small></span>
            <button className="btn small" type="button" disabled={!pushAvailable() || pushOn} onClick={() => void enablePush()}>
              {t('push.enable')}
            </button>
          </div>
        </div>
      </section>
      <section className="settings-section">
        <h2>{t('settings.data')}</h2>
        <div className="settings-card">
          <div className="settings-row">
            <span className="label">{t('settings.cache')}<small>{t('settings.cache.hint')}</small></span>
            <button className="btn small" type="button" onClick={() => { clearLocalData(); toast(t('settings.saved')); }}>{t('settings.clear')}</button>
          </div>
          <div className="settings-row">
            <span className="label">{t('settings.logout')}</span>
            <button className="btn small danger" type="button" onClick={() => void logout()}>{t('settings.logout')}</button>
          </div>
        </div>
      </section>
      <section className="settings-section">
        <h2>{t('settings.about')}</h2>
        <div className="settings-card"><div className="settings-row"><span className="label">UFOT · Unfettered Freedom Of Thought</span><span className="tag">{t('settings.version')}</span></div></div>
      </section>
    </main>
  );
}
