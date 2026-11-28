/** 联系人：搜索用户（940 op5）、按用户 ID 发起单聊、创建群聊。 */

import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { Avatar, Button, EmptyState, Field, TextInput } from '@/ui/primitives/controls';
import { ConvType } from '@/core/domain/conv';
import { requestSearchUsers, sendCreateConversation } from '@/core/store/bridge';
import { useUiStore } from '@/core/store/ui';
import type { UserProfile } from '@/core/domain/types';

export function ContactsPage() {
  const navigate = useNavigate();
  const pushToast = useUiStore((s) => s.pushToast);
  const [query, setQuery] = useState('');
  const [users, setUsers] = useState<UserProfile[]>([]);
  const [searching, setSearching] = useState(false);
  const [manualId, setManualId] = useState('');
  const [groupTitle, setGroupTitle] = useState('');

  // 用户搜索：940 op5（WS 通道），输入停顿 300ms 后发一次。
  useEffect(() => {
    const keyword = query.trim();
    if (!keyword) {
      setUsers([]);
      return;
    }
    let cancelled = false;
    const timer = setTimeout(() => {
      setSearching(true);
      requestSearchUsers(keyword)
        .then((result) => {
          if (cancelled) return;
          const body = result.body as { items?: UserProfile[] } | undefined;
          setUsers(body?.items ?? []);
        })
        .catch((error: unknown) => {
          if (cancelled) return;
          pushToast(error instanceof Error ? error.message : '搜索失败', 'error');
        })
        .finally(() => {
          if (!cancelled) setSearching(false);
        });
    }, 300);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [query, pushToast]);

  const startChat = (memberIds: number[], title?: string) => {
    const isGroup = Boolean(title && title.trim());
    const ok = sendCreateConversation({
      title: title?.trim() ?? '',
      convType: isGroup ? ConvType.GROUP : ConvType.DIRECT,
      memberIds,
    });
    if (!ok) {
      pushToast('连接不可用，暂时无法创建会话', 'error');
      return;
    }
    pushToast(isGroup ? '正在创建群聊…' : '正在创建会话…', 'info');
    navigate('/chats');
  };

  return (
    <div className="uf-page">
      <header className="uf-topbar">
        <span className="uf-topbar__title">联系人</span>
      </header>

      <div className="uf-page__body">
        <div className="uf-card">
          <Field label="搜索用户" hint="按昵称检索">
            {(id) => (
              <TextInput
                id={id}
                value={query}
                placeholder="输入昵称关键字"
                onChange={(event) => setQuery(event.target.value)}
              />
            )}
          </Field>
          {searching ? <div className="uf-faint">搜索中…</div> : null}
        </div>

        {query.trim() ? (
          <div className="uf-card">
            <strong>搜索结果</strong>
            {users.length === 0 && !searching ? (
              <EmptyState icon="user" title="没有匹配的用户" />
            ) : null}
            {users.map((user) => (
              <div key={user.user_id} className="uf-row" style={{ marginTop: 'var(--sp-3)' }}>
                <Avatar name={user.display_name} />
                <div className="uf-col uf-grow">
                  <span className="truncate">{user.display_name || `用户 ${user.user_id}`}</span>
                  <span className="uf-faint">ID {user.user_id}</span>
                </div>
                <Button small onClick={() => startChat([user.user_id])}>
                  发起会话
                </Button>
              </div>
            ))}
          </div>
        ) : null}

        <div className="uf-card">
          <div className="uf-row" style={{ marginBottom: 'var(--sp-2)' }}>
            <Icon name="link" />
            <strong>按用户 ID 发起会话</strong>
          </div>
          <Field label="对方用户 ID">
            {(id) => (
              <TextInput
                id={id}
                value={manualId}
                inputMode="numeric"
                placeholder="例如 12"
                onChange={(event) => setManualId(event.target.value)}
              />
            )}
          </Field>
          <Button
            variant="primary"
            style={{ marginTop: 'var(--sp-3)' }}
            disabled={!manualId}
            onClick={() => startChat([Number(manualId)])}
          >
            发起单聊
          </Button>
        </div>

        <div className="uf-card">
          <div className="uf-row" style={{ marginBottom: 'var(--sp-2)' }}>
            <Icon name="group" />
            <strong>新建群聊</strong>
          </div>
          <Field label="群名称">
            {(id) => (
              <TextInput
                id={id}
                value={groupTitle}
                maxLength={30}
                onChange={(event) => setGroupTitle(event.target.value)}
              />
            )}
          </Field>
          <Button
            style={{ marginTop: 'var(--sp-3)' }}
            disabled={!groupTitle.trim()}
            onClick={() => startChat([], groupTitle.trim())}
          >
            创建群聊
          </Button>
        </div>
      </div>
    </div>
  );
}
