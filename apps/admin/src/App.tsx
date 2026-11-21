import { useCallback, useEffect, useState } from 'react';
import { useAuth } from './auth';
import { Login } from './ui/views/Login';
import { Shell } from './ui/layout/Shell';
import { findNav, NAV_ITEMS } from './ui/layout/nav';
import { Dashboard } from './ui/views/Dashboard';
import { Metrics } from './ui/views/Metrics';
import { Users } from './ui/views/Users';
import { Reports } from './ui/views/Reports';
import { Messages } from './ui/views/Messages';
import { Flags } from './ui/views/Flags';
import { Broadcast } from './ui/views/Broadcast';
import { Audit } from './ui/views/Audit';
import { Archive } from './ui/views/Archive';

/** 极简 hash 路由：#/users；不引入 router 依赖，后台路径是静态的。 */
function useHashRoute(): [string, (path: string) => void] {
  const read = () => {
    const raw = window.location.hash.replace(/^#/, '');
    const path = raw.split('?')[0] ?? '';
    return NAV_ITEMS.some((item) => item.path === path) ? path : '/dashboard';
  };
  const [path, setPath] = useState(read);

  useEffect(() => {
    const onChange = () => setPath(read());
    window.addEventListener('hashchange', onChange);
    if (!window.location.hash) window.location.replace('#/dashboard');
    return () => window.removeEventListener('hashchange', onChange);
  }, []);

  const navigate = useCallback((next: string) => {
    if (window.location.hash === `#${next}`) return;
    window.location.hash = next;
  }, []);

  return [path, navigate];
}

function Splash() {
  return (
    <div className="flex h-full items-center justify-center bg-bg-page">
      <div className="flex items-center gap-3 text-text-secondary">
        <span className="h-2.5 w-2.5 animate-pulse rounded-full bg-primary-500" />
        <span className="text-table">正在校验管理会话…</span>
      </div>
    </div>
  );
}

function View({ path }: { path: string }) {
  switch (path) {
    case '/dashboard':
      return <Dashboard />;
    case '/users':
      return <Users />;
    case '/reports':
      return <Reports />;
    case '/messages':
      return <Messages />;
    case '/flags':
      return <Flags />;
    case '/broadcast':
      return <Broadcast />;
    case '/audit':
      return <Audit />;
    case '/archive':
      return <Archive />;
    case '/metrics':
    default:
      return <Metrics />;
  }
}

export function App() {
  const { status } = useAuth();
  const [path, navigate] = useHashRoute();

  useEffect(() => {
    document.title = `${findNav(path).label} · UFOT 管理后台`;
  }, [path]);

  if (status === 'loading') return <Splash />;
  if (status === 'anon') return <Login />;

  return (
    <Shell path={path} onNavigate={navigate}>
      <View path={path} />
    </Shell>
  );
}
