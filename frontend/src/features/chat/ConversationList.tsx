/** 会话列表：搜索、置顶、静音、未读、最后一条预览、新建入口。 */

import { useEffect, useMemo } from 'react';
import { useNavigate } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { Avatar, Badge, Button, EmptyState, IconButton, TextInput } from '@/ui/primitives/controls';
import { useConvStore } from '@/core/store/conv';
import { conversationFromPayload } from '@/core/store/conv';
import { useAuthStore } from '@/core/store/auth';
import { useUiStore } from '@/core/store/ui';
import { requestListMyConversations } from '@/core/store/bridge';

function formatTime(ts?: number): string {
  if (!ts) return '';
  const date = new Date(ts);
  const now = new Date();
  const sameDay = date.toDateString() === now.toDateString();
  if (sameDay) {
    return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
  }
  if (date.getFullYear() === now.getFullYear()) {
    return `${date.getMonth() + 1}/${date.getDate()}`;
  }
  return `${date.getFullYear()}/${date.getMonth() + 1}/${date.getDate()}`;
}

export function ConversationList() {
  const navigate = useNavigate();
  const query = useConvStore((s) => s.query);
  const setQuery = useConvStore((s) => s.setQuery);
  const replaceAll = useConvStore((s) => s.replaceAll);
  const loaded = useConvStore((s) => s.loaded);
  const loading = useConvStore((s) => s.loading);
  const userId = useAuthStore((s) => s.session?.user_id ?? 0);
  const pushToast = useUiStore((s) => s.pushToast);
  const visible = useConvStore((s) => s.visible);

  // 权威列表：930 op7「列出我的会话」。失败只提示，不清空已有数据。
  const refresh = async () => {
    useConvStore.setState({ loading: true });
    try {
      const result = await requestListMyConversations();
      const body = result.body as { items?: Record<string, unknown>[] } | undefined;
      const items = (body?.items ?? [])
        .map((item) => conversationFromPayload(item))
        .filter((c): c is NonNullable<typeof c> => Boolean(c));
      replaceAll(items);
    } catch (error) {
      pushToast(
        error instanceof Error ? `会话列表加载失败：${error.message}` : '会话列表加载失败',
        'error',
      );
    } finally {
      useConvStore.setState({ loading: false });
    }
  };

  useEffect(() => {
    void refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const items = visible();
  const pinned = useMemo(() => items.filter((c) => c.pinned), [items]);
  const others = useMemo(() => items.filter((c) => !c.pinned), [items]);

  const renderItem = (
    convId: number,
    title: string,
    lastText: string,
    time: number,
    unread: number,
    muted: boolean,
    isPinned: boolean,
  ) => (
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
        <IconButton icon="refresh" label="刷新会话" onClick={() => void refresh()} />
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
          loaded ? (
            <EmptyState
              icon="chat"
              title="还没有会话"
              description="点右上角 + 发起一个会话，收到的消息会出现在这里。"
              action={
                <Button variant="primary" icon="plus" onClick={() => navigate('/contacts')}>
                  新建会话
                </Button>
              }
            />
          ) : (
            <EmptyState icon="chat" title="正在加载会话…" />
          )
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
        {loading && items.length > 0 ? (
          <div className="uf-faint" style={{ padding: 'var(--sp-2) var(--sp-3)' }}>
            正在刷新…
          </div>
        ) : null}
      </div>

      <div className="uf-list__footer">
        <span className="uf-faint">用户 ID：{userId || '—'}</span>
      </div>
    </>
  );
}
