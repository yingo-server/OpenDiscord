/** 会话列表：搜索、置顶、静音、未读、最后一条预览。 */

import { useEffect, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { Avatar, Badge, EmptyState, IconButton, TextInput } from '@/ui/primitives/controls';
import { useConvStore } from '@/core/store/conv';
import { useAuthStore } from '@/core/store/auth';
function formatTime(ts?: number): string {
  if (!ts) return '';
  const date = new Date(ts);
  const now = new Date();
  const sameDay = date.toDateString() === now.toDateString();
  if (sameDay) {
    return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
  }
  return `${date.getMonth() + 1}/${date.getDate()}`;
}

export function ConversationList() {
  const navigate = useNavigate();
  const query = useConvStore((s) => s.query);
  const setQuery = useConvStore((s) => s.setQuery);
  const load = useConvStore((s) => s.load);
  const listEndpointReady = useConvStore((s) => s.listEndpointReady);
  const userId = useAuthStore((s) => s.session?.user_id ?? 0);
  const visible = useConvStore((s) => s.visible);

  useEffect(() => {
    void load();
  }, [load]);

  const items = visible();
  const pinned = useMemo(() => items.filter((c) => c.pinned), [items]);
  const others = useMemo(() => items.filter((c) => !c.pinned), [items]);

  const renderItem = (convId: number, title: string, lastText: string, time: number, unread: number, muted: boolean, isPinned: boolean) => (
    <button
      key={convId}
      type="button"
      className="uf-conv"
      onClick={() => navigate(`/chats/${convId}`)}
    >
      <Avatar name={title || '会话'} />
      <span className="uf-conv__body">
        <span className="uf-conv__row">
          <span className="uf-conv__title truncate">{title || `会话 ${convId}`}</span>
          <span className="uf-faint">{formatTime(time)}</span>
        </span>
        <span className="uf-conv__row">
          <span className="uf-conv__preview truncate">{lastText || '暂无消息'}</span>
          {muted ? <Icon name="bell" size="sm" className="uf-faint" /> : null}
          {isPinned ? <Icon name="list" size="sm" className="uf-faint" /> : null}
          {unread > 0 ? <Badge count={unread} tone="blue" /> : null}
        </span>
      </span>
    </button>
  );

  return (
    <>
      <div className="uf-list__top">
        <span className="uf-grow uf-topbar__title">会话</span>
        <IconButton icon="plus" label="新建会话" onClick={() => navigate('/contacts')} />
      </div>

      <div className="uf-list__search">
        <TextInput
          value={query}
          placeholder="搜索会话"
          onChange={(event) => setQuery(event.target.value)}
          aria-label="搜索会话"
        />
      </div>

      <div className="uf-list__scroll">
        {items.length === 0 ? (
          <EmptyState
            icon="chat"
            title="还没有会话"
            description={
              listEndpointReady === false
                ? '后端暂未开放会话列举接口；新建会话并收到消息后会出现在这里。'
                : '新建一个会话开始聊天。'
            }
          />
        ) : (
          <>
            {pinned.length > 0 ? (
              <>
                <div className="uf-list__section">置顶</div>
                {pinned.map((conv) =>
                  renderItem(
                    conv.convId,
                    conv.title,
                    conv.lastMessage?.text ?? '',
                    conv.lastActivity ?? conv.lastSeq ?? 0,
                    conv.unread ?? 0,
                    Boolean(conv.muted),
                    true,
                  ),
                )}
              </>
            ) : null}
            {others.length > 0 ? (
              <>
                {pinned.length > 0 ? <div className="uf-list__section">全部</div> : null}
                {others.map((conv) =>
                  renderItem(
                    conv.convId,
                    conv.title,
                    conv.lastMessage?.text ?? '',
                    conv.lastActivity ?? conv.lastSeq ?? 0,
                    conv.unread ?? 0,
                    Boolean(conv.muted),
                    false,
                  ),
                )}
              </>
            ) : null}
          </>
        )}
      </div>

      <div className="uf-list__footer">
        <div className="uf-row">
          <span className="uf-faint uf-grow">用户 ID：{userId || '—'}</span>
          <IconButton icon="refresh" label="刷新会话" onClick={() => void load()} />
        </div>
      </div>
    </>
  );
}
