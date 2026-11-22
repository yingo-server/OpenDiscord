import { useEffect, useState, type ReactNode } from 'react';
import { getSession } from '../state/session';
import { getAppServices } from '../app/services';
import { env } from '../platform/env';
import { callSession } from '../net/ws/call';
import { t } from './i18n';
import { cn } from './lib/format';
import { ConvList } from './components/ConvList';
import { MessageList, useChatState } from './components/MessageList';
import { Composer } from './components/Composer';
import { CallOverlay } from './components/CallOverlay';
import { LoginScreen } from './views/Login';
import { Settings } from './views/Settings';
import { BrandMark } from './components/BrandMark';

type View = 'chats' | 'settings';

function ChatPane({ convId, onBack }: { convId: number; onBack: () => void }): ReactNode {
  const state = useChatState();
  const conv = state.conversations.find((candidate) => candidate.id === convId);
  const canCall = conv?.type === 'direct' && conv.memberIds.some((id) => id !== (getAppServices().session()?.user_id || 0)) && callSession.enabled;
  return (
    <section className={cn('chat-pane', convId === 0 && 'hidden-mobile')}>
      <header className="chat-head">
        <button className="icon-btn" type="button" onClick={onBack} aria-label={t('common.close')}>←</button>
        <div><h2>{conv?.title || t('nav.chats')}</h2><div className="sub">{conv?.type === 'group' ? t('conv.group') : t('conv.direct')}</div></div>
        {canCall ? (
          <button
            className="icon-btn chat-call"
            type="button"
            aria-label={t('call.title')}
            title={callSession.call.state !== 'idle' ? undefined : t('call.title')}
            disabled={callSession.call.state !== 'idle'}
            onClick={() => {
              const peer = conv?.memberIds.find((id) => id !== (getAppServices().session()?.user_id || 0));
              if (conv && peer) void callSession.start(conv.id, peer, conv.title).catch(() => undefined);
            }}
          >◌</button>
        ) : null}
      </header>
      <MessageList convId={convId} state={state} />
      <div className="typing-line">{state.typing[convId]?.length ? t('chat.typing') : ''}</div>
      <Composer convId={convId} />
    </section>
  );
}

function ConnectionBar(): ReactNode {
  const [label, setLabel] = useState('');
  useEffect(() => {
    if (env.mock) return; // mock 模式无真实连接，不显示网络条
    const handler = (event: Event) => {
      const state = (event as CustomEvent<{ state: string }>).detail?.state;
      if (state === 'online') setLabel('');
      else if (state === 'banned') setLabel(t('error.banned.title'));
      else if (state === 'connecting') setLabel(t('net.reconnecting'));
      else setLabel(t('net.offline'));
    };
    window.addEventListener('ufot:connection', handler);
    return () => window.removeEventListener('ufot:connection', handler);
  }, []);
  if (!label || env.mock) return null;
  return <div className="net-bar offline" role="status">{label}</div>;
}

export function App(): ReactNode {
  const session = getSession();
  const state = useChatState();
  const [view, setView] = useState<View>('chats');
  const services = getAppServices();
  if (!session) return <LoginScreen />;

  const activeConvId = state.activeConvId ?? 0;

  return (
    <div className="app-shell">
      <nav className="rail" aria-label={t('nav.chats')}>
        <div className="brand"><BrandMark size={30} /><div><div className="brand-name">UFOT</div><div className="brand-sub">Unfettered Freedom Of Thought</div></div></div>
        <div className="rail-nav">
          <button type="button" className={cn('rail-item', view === 'chats' && 'active')} onClick={() => setView('chats')}>
            <span>▤</span><span>{t('nav.chats')}</span>{state.unreadTotal > 0 ? <span className="badge">{state.unreadTotal > 99 ? '99+' : state.unreadTotal}</span> : null}
          </button>
          <button type="button" className={cn('rail-item', view === 'settings' && 'active')} onClick={() => setView('settings')}><span>⚙</span><span>{t('nav.settings')}</span></button>
        </div>
        <div className="rail-bottom"><span className="brand-sub">{session.guest ? t('auth.guest') : session.profile.display_name || t('settings.account')}</span></div>
      </nav>
      <div className="main">
        <ConnectionBar />
        {view === 'settings' ? <Settings onBack={() => setView('chats')} /> : (
          <div className="workspace">
            <ConvList state={state} onSelect={(id) => services.selectConversation(id)} onOpenSettings={() => setView('settings')} />
            <ChatPane convId={activeConvId} onBack={() => services.selectConversation(null)} />
          </div>
        )}
      </div>
      <CallOverlay />
    </div>
  );
}
