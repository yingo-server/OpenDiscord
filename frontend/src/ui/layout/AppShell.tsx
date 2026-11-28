/**
 * 应用外壳：侧边导航（桌面）、底部标签栏（手机）、连接状态指示。
 * 会话区的“列表 + 详情”两栏由 features/chat/ChatLayout 承担，
 * 这样外壳只负责导航与全局状态，避免出现两个 <Outlet/> 渲染同一子树。
 */

import { NavLink, Outlet } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { useConnStore } from '@/core/store/bridge';
import { useUiStore } from '@/core/store/ui';

export function AppShell() {
  const state = useConnStore((s) => s.state);
  const latency = useConnStore((s) => s.latencyMs);
  const pushToast = useUiStore((s) => s.pushToast);

  const connLabel =
    state === 'open'
      ? '已连接'
      : state === 'connecting'
        ? '连接中'
        : state === 'reconnecting'
          ? '重连中'
          : state === 'failed'
            ? '连接失败'
            : '未连接';

  return (
    <div className="uf-shell">
      <nav className="uf-rail" aria-label="主导航">
        <span className="uf-rail__logo" title="UFOT">
          UF
        </span>
        <NavLink to="/chats" className="uf-rail__item" aria-label="会话">
          <Icon name="chat" />
        </NavLink>
        <NavLink to="/contacts" className="uf-rail__item" aria-label="联系人">
          <Icon name="userPlus" />
        </NavLink>
        <NavLink to="/me" className="uf-rail__item" aria-label="我的">
          <Icon name="user" />
        </NavLink>
        <div className="uf-grow" />
        <button
          type="button"
          className="uf-rail__item"
          aria-label={`连接状态：${connLabel}`}
          title={`${connLabel}${latency ? ` · ${latency}ms` : ''}`}
          onClick={() =>
            pushToast(
              state === 'open'
                ? `WebSocket 已连接${latency ? `（延迟 ${latency}ms）` : ''}`
                : `WebSocket ${connLabel}`,
              state === 'open' ? 'info' : 'warn',
            )
          }
        >
          <Icon name="online" />
        </button>
      </nav>

      <main className="uf-main">
        <Outlet />
      </main>

      <nav className="uf-bottomtabs" aria-label="底部导航">
        <NavLink to="/chats" className="uf-bottomtabs__item">
          <Icon name="chat" />
          <span>会话</span>
        </NavLink>
        <NavLink to="/contacts" className="uf-bottomtabs__item">
          <Icon name="userPlus" />
          <span>联系人</span>
        </NavLink>
        <NavLink to="/me" className="uf-bottomtabs__item">
          <Icon name="user" />
          <span>我的</span>
        </NavLink>
        <button
          type="button"
          className="uf-bottomtabs__item"
          onClick={() => pushToast('管理员入口：/admin', 'info')}
        >
          <Icon name="shield" />
          <span>管理</span>
        </button>
      </nav>
    </div>
  );
}
