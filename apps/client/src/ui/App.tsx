/**
 * 应用外壳：NavigationView 风格（左侧 Acrylic 导航栏 + 内容区），
 * hash 路由 + 会话列表/聊天窗两栏（移动单栏）+ 登录/封禁门禁。
 * 路由判断与所有 view 挂载点保持原样——本文件只换视觉外壳。
 * 移动端进入聊天后卸载会话列表（虚拟列表销毁），返回时重建。
 */
import { useEffect, type ReactNode } from 'react';
import { useRoute, navigate, matchChat } from './router';
import { useSessionStore } from '../core/stores/session-store';
import { ConvList } from './views/conv-list/ConvList';
import { ChatView } from './views/chat/ChatView';
import { Settings } from './views/settings/Settings';
import { Banned } from './views/banned/Banned';
import { LoginScreen } from './views/login/LoginScreen';
import { Toaster } from './components/Toast';
import { CallOverlay } from './views/call/CallOverlay';
import { ChatIcon, LogoMark, SettingsIcon } from './components/icons';

export function App() {
  const path = useRoute();
  const status = useSessionStore((state) => state.status);

  useEffect(() => {
    if (status === 'banned') {
      if (path !== '/banned') navigate('/banned');
      return;
    }
    if (status === 'anonymous') {
      if (path !== '/login') navigate('/login');
      return;
    }
    if (path === '/' || path === '/login' || path === '/banned') navigate('/chats');
  }, [status, path]);

  if (status === 'banned') {
    return (
      <>
        <Banned />
        <Toaster />
      </>
    );
  }

  if (status === 'anonymous') {
    return (
      <>
        <LoginScreen />
        <Toaster />
      </>
    );
  }

  if (path === '/settings') {
    return (
      <NavShell>
        <div className="min-h-0 min-w-0 flex-1">
          <Settings />
        </div>
      </NavShell>
    );
  }

  const convId = matchChat(path);
  const chatOpen = convId !== null;

  return (
    <NavShell>
      <div className="flex min-h-0 min-w-0 flex-1">
        <ConvList
          className={`${chatOpen ? 'hidden tablet:flex' : 'flex'} w-full shrink-0 border-e border-line bg-surface tablet:w-sidebar`}
        />
        <main
          className={`${chatOpen ? 'flex' : 'hidden tablet:flex'} min-h-0 min-w-0 flex-1 flex-col bg-page`}
          aria-label={chatOpen ? undefined : 'chat'}
        >
          <ChatView convId={chatOpen ? convId : null} />
        </main>
      </div>
      <CallOverlay />
    </NavShell>
  );
}

/**
 * NavigationView 外壳：
 * - 桌面：左侧 72px Acrylic 导航栏（品牌字标 + 图标按钮），右侧内容区；
 * - 移动：折叠为顶部 48px Acrylic 标题栏，导航项收进标题栏；
 * - 所有内容通过 children 挂载，路由逻辑不进入本组件。
 */
function NavShell({ children }: { children: ReactNode }) {
  const path = useRoute();
  const settingsOpen = path === '/settings';

  return (
    <div className="flex h-[100dvh] overflow-hidden bg-surface-mica">
      <nav
        aria-label="主导航"
        className="acrylic hidden w-[72px] shrink-0 flex-col items-center gap-2 border-e border-line pb-4 pt-safe tablet:flex"
      >
        <div className="flex h-14 w-full items-center justify-center" aria-hidden="true">
          <LogoMark size={40} />
        </div>
        <NavButton
          active={!settingsOpen}
          label="聊天"
          onClick={() => navigate('/chats')}
          icon={<ChatIcon size={22} />}
        />
        <NavButton
          active={settingsOpen}
          label="设置"
          onClick={() => navigate('/settings')}
          icon={<SettingsIcon size={22} />}
        />
      </nav>

      <div className="flex min-h-0 min-w-0 flex-1 flex-col">
        {settingsOpen ? null : (
        <header className="acrylic flex h-12 shrink-0 items-center gap-3 border-b border-line px-3 tablet:hidden">
          <LogoMark size={28} />
          <span className="font-display text-title font-semibold text-ink">UFOT</span>
          <div className="ms-auto flex items-center gap-1">
            <NavButton
              active={!settingsOpen}
              label="聊天"
              onClick={() => navigate('/chats')}
              icon={<ChatIcon size={20} />}
              compact
            />
            <NavButton
              active={settingsOpen}
              label="设置"
              onClick={() => navigate('/settings')}
              icon={<SettingsIcon size={20} />}
              compact
            />
          </div>
        </header>
        )}
        {children}
      </div>
      <Toaster />
    </div>
  );
}

function NavButton({
  active,
  label,
  icon,
  onClick,
  compact = false,
}: {
  active: boolean;
  label: string;
  icon: ReactNode;
  onClick: () => void;
  compact?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      aria-current={active ? 'page' : undefined}
      title={label}
      className={[
        'relative inline-flex shrink-0 items-center justify-center text-ink-secondary',
        'transition-colors duration-micro ease-out hover:bg-ink/[0.05] hover:text-ink',
        'focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-focus',
        compact ? 'h-9 w-9 rounded-md' : 'h-11 w-11 rounded-md',
        active ? 'text-primary-500' : '',
        active ? 'before:absolute before:inset-0 before:bg-primary-500/10' : '',
      ].join(' ')}
    >
      {icon}
      {active && !compact ? (
        <span className="absolute -start-3 top-1/2 h-5 w-[3px] -translate-y-1/2 rounded-full bg-primary-500" aria-hidden="true" />
      ) : null}
    </button>
  );
}
