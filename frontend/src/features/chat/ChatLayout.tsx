/**
 * 会话区：左侧列表 + 右侧详情。
 * 桌面/平板宽屏两栏常驻；平板窄屏与手机按 data-pane 在列表/详情之间切换。
 */

import { Outlet, useLocation } from 'react-router-dom';
import { ConversationList } from './ConversationList';

export function ChatLayout() {
  const location = useLocation();
  const inChat = /^\/chats\/[^/]+/.test(location.pathname);
  const pane = inChat ? 'chat' : 'list';

  return (
    <div className="uf-chats" data-pane={pane}>
      <aside className="uf-list" aria-label="会话列表">
        <ConversationList />
      </aside>
      <section className="uf-detail" aria-label="会话内容">
        <Outlet />
      </section>
    </div>
  );
}
