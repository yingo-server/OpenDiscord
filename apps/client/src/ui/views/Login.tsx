import { useState, type FormEvent, type ReactNode } from 'react';
import { ApiError } from '../../net/http/client';
import { getAppServices } from '../../app/services';
import { parseSession } from '../../domain/parse';
import { t } from '../i18n';

type Tab = 'phone' | 'email' | 'password' | 'guest';

const TABS: Array<{ id: Tab; label: string }> = [
  { id: 'phone', label: 'auth.phone' },
  { id: 'email', label: 'auth.email' },
  { id: 'password', label: 'auth.password' },
  { id: 'guest', label: 'auth.guest' },
];

export function LoginScreen(): ReactNode {
  const [tab, setTab] = useState<Tab>('guest');
  const [phone, setPhone] = useState('');
  const [code, setCode] = useState('');
  const [email, setEmail] = useState('');
  const [account, setAccount] = useState('');
  const [password, setPassword] = useState('');
  const [nickname, setNickname] = useState('');
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState<{ kind: 'info' | 'error'; text: string } | null>(null);
  const [codeSent, setCodeSent] = useState(false);
  const [linkSent, setLinkSent] = useState(false);
  const services = getAppServices();

  async function submit(event: FormEvent): Promise<void> {
    event.preventDefault();
    setNotice(null);
    setBusy(true);
    try {
      let session: unknown;
      if (tab === 'phone') {
        session = await services.auth.phoneVerify(phone.trim(), code.trim());
      } else if (tab === 'email') {
        const result = await services.auth.emailPoll(email.trim());
        if (result && typeof result === 'object' && 'status' in result) {
          setNotice({ kind: 'info', text: t('auth.polling') });
          return;
        }
        session = result;
      } else if (tab === 'password') {
        session = await services.auth.password(account.trim(), password);
      } else {
        session = await services.auth.guest(nickname.trim() || undefined);
      }
      const parsed = parseSession(session);
      if (!parsed) throw new ApiError('BAD_RESPONSE', t('error.generic'));
      services.authenticate(parsed);
    } catch (error) {
      setNotice({ kind: 'error', text: error instanceof ApiError ? error.message : t('error.generic') });
    } finally {
      setBusy(false);
    }
  }

  async function sendCode(): Promise<void> {
    setNotice(null);
    setBusy(true);
    try {
      await services.auth.phoneCode(phone.trim());
      setCodeSent(true);
      setNotice({ kind: 'info', text: t('auth.code.sent') });
    } catch (error) {
      setNotice({ kind: 'error', text: error instanceof ApiError ? error.message : t('error.generic') });
    } finally {
      setBusy(false);
    }
  }

  async function sendLink(): Promise<void> {
    setNotice(null);
    setBusy(true);
    try {
      await services.auth.emailLink(email.trim());
      setLinkSent(true);
      setNotice({ kind: 'info', text: t('auth.link.sent') });
    } catch (error) {
      setNotice({ kind: 'error', text: error instanceof ApiError ? error.message : t('error.generic') });
    } finally {
      setBusy(false);
    }
  }

  return (
    <main className="center-page">
      <div className="auth-card">
        <div className="auth-mark">
          <div className="brand-mark">UF</div>
          <div><h1>{t('app.name')}</h1><p>Unfettered Freedom Of Thought</p></div>
        </div>
        <div className="card">
          <div className="auth-tabs" role="tablist" aria-label={t('auth.title')}>
            {TABS.map((item) => (
              <button
                key={item.id}
                type="button"
                role="tab"
                aria-selected={tab === item.id}
                className={tab === item.id ? 'auth-tab active' : 'auth-tab'}
                onClick={() => { setTab(item.id); setNotice(null); }}
              >{t(item.label)}</button>
            ))}
          </div>
          {notice ? <div className={`notice ${notice.kind}`} role="status">{notice.text}</div> : null}
          <form onSubmit={(event) => void submit(event)}>
            {tab === 'phone' ? (
              <>
                <p className="field hint">{t('auth.phone.hint')}</p>
                <div className="field"><label htmlFor="phone">{t('auth.phone.number')}</label><input id="phone" className="input" type="tel" value={phone} onChange={(event) => setPhone(event.target.value)} required placeholder="+8613800138000" /></div>
                <div className="field"><label htmlFor="code">{t('auth.code')}</label><input id="code" className="input" inputMode="numeric" value={code} onChange={(event) => setCode(event.target.value)} required /></div>
                <button className="btn small" type="button" disabled={busy || !phone.trim()} onClick={() => void sendCode()}>{codeSent ? t('auth.resend') : t('auth.send.code')}</button>
              </>
            ) : null}
            {tab === 'email' ? (
              <>
                <p className="field hint">{t('auth.email.hint')}</p>
                <div className="field"><label htmlFor="email">{t('auth.email.address')}</label><input id="email" className="input" type="email" value={email} onChange={(event) => setEmail(event.target.value)} required /></div>
                <button className="btn small" type="button" disabled={busy || !email.trim()} onClick={() => void sendLink()}>{linkSent ? t('auth.resend') : t('auth.send.link')}</button>
              </>
            ) : null}
            {tab === 'password' ? (
              <>
                <p className="field hint">{t('auth.password.hint')}</p>
                <div className="field"><label htmlFor="account">{t('auth.account')}</label><input id="account" className="input" value={account} onChange={(event) => setAccount(event.target.value)} required /></div>
                <div className="field"><label htmlFor="password">{t('auth.password.field')}</label><input id="password" className="input" type="password" value={password} onChange={(event) => setPassword(event.target.value)} required /></div>
              </>
            ) : null}
            {tab === 'guest' ? (
              <>
                <p className="field hint">{t('auth.guest.hint')}</p>
                <div className="field"><label htmlFor="nickname">{t('settings.display.name')}</label><input id="nickname" className="input" value={nickname} onChange={(event) => setNickname(event.target.value)} placeholder={t('auth.guest')} /></div>
              </>
            ) : null}
            <button className="btn primary auth-submit" type="submit" disabled={busy}>{busy ? t('common.loading') : tab === 'guest' ? t('auth.enter.guest') : t('auth.sign.in')}</button>
          </form>
        </div>
      </div>
    </main>
  );
}
