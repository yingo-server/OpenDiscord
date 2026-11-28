/** 联系人：搜索用户、发起单聊/群聊。 */

import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { Icon } from '@/ui/primitives/Icon';
import { Avatar, Button, EmptyState, Field, TextInput } from '@/ui/primitives/controls';
import { isEndpointMissing, searchUsers } from '@/core/domain/conv';
import { sendCreateConversation } from '@/core/store/bridge';
import { useUiStore } from '@/core/store/ui';
import type { UserProfile } from '@/core/domain/types';

export function ContactsPage() {
  const navigate = useNavigate();
  const pushToast = useUiStore((s) => s.pushToast);
  const [query, setQuery] = useState('');
  const [users, setUsers] = useState<UserProfile[]>([]);
  const [endpointMissing, setEndpointMissing] = useState(false);
  const [manualId, setManualId] = useState('');
  const [groupTitle, setGroupTitle] = useState('');

  useEffect(() => {
    let cancelled = false;
    const timer = setTimeout(() => {
      void searchUsers({ q: query, limit: 30 })
        .then((page) => {
          if (cancelled) return;
          setUsers(page.items);
          setEndpointMissing(false);
        })
        .catch((error) => {
          if (cancelled) return;
          if (isEndpointMissing(error)) {
            setEndpointMissing(true);
            setUsers([]);
          } else {
            pushToast(error instanceof Error ? error.message : '搜索失败', 'error');
          }
        });
    }, 250);
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [query, pushToast]);

  const startChat = (memberIds: number[], title?: string) => {
    const ok = sendCreateConversation({
      title: title ?? '',
      convType: memberIds.length > 1 ? 1 : 0,
      memberIds,
    });
    if (!ok) {
      pushToast('连接不可用，无法创建会话', 'error');
      return;
    }
    pushToast('已发出创建会话请求', 'info');
  };

  return (
    <div className="uf-page">
      <header className="uf-topbar">
        <span className="uf-topbar__title">联系人</span>
      </header>

      <div className="uf-page__body">
        <div className="uf-card">
          <Field label="搜索用户" hint="按昵称或用户 ID 检索">
            {(id) => (
              <TextInput
                id={id}
                value={query}
                placeholder="输入关键字"
                onChange={(event) => setQuery(event.target.value)}
              />
            )}
          </Field>
        </div>

        {endpointMissing ? (
          <div className="uf-card">
            <div className="uf-row" style={{ marginBottom: 'var(--sp-2)' }}>
              <Icon name="info" />
              <strong>后端尚未开放用户检索接口</strong>
            </div>
            <div className="uf-faint" style={{ marginBottom: 'var(--sp-3)' }}>
              可以直接输入对方用户 ID 发起单聊，或创建群聊。
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
        ) : null}

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
                maxLength={64}
                onChange={(event) => setGroupTitle(event.target.value)}
              />
            )}
          </Field>
          <Button
            style={{ marginTop: 'var(--sp-3)' }}
            disabled={!groupTitle.trim()}
            onClick={() => startChat([], groupTitle.trim())}
          >
            创建空群聊
          </Button>
        </div>

        <div className="uf-card">
          <strong>用户</strong>
          {users.length === 0 && !endpointMissing ? (
            <EmptyState icon="user" title="没有匹配的用户" />
          ) : null}
          {users.map((user) => (
            <div key={user.user_id} className="uf-row" style={{ marginTop: 'var(--sp-3)' }}>
              <Avatar name={user.display_name} />
              <div className="uf-col uf-grow">
                <span className="truncate">{user.display_name || `用户 ${user.user_id}`}</span>
                <span className="uf-faint">ID {user.user_id}</span>
              </div>
              <Button
                small
                onClick={() => {
                  startChat([user.user_id]);
                  navigate('/chats');
                }}
              >
                发起会话
              </Button>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}
