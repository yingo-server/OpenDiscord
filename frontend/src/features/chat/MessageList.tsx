/** 消息流：按天分组、气泡、媒体预览、状态标记、消息操作。 */

import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { Icon } from '@/ui/primitives/Icon';
import { IconButton } from '@/ui/primitives/controls';
import { useReplyStore } from './replyStore';
import type { ChatMessage } from '@/core/domain/types';

function formatTime(ts: number): string {
  const date = new Date(ts);
  return `${String(date.getHours()).padStart(2, '0')}:${String(date.getMinutes()).padStart(2, '0')}`;
}

function formatDay(ts: number): string {
  const date = new Date(ts);
  const today = new Date();
  const yesterday = new Date(today.getTime() - 86_400_000);
  if (date.toDateString() === today.toDateString()) return '今天';
  if (date.toDateString() === yesterday.toDateString()) return '昨天';
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`;
}

function statusIcon(status: ChatMessage['status']) {
  if (status === 'failed') return <Icon name="warning" size="sm" style={{ color: 'var(--red)' }} />;
  if (status === 'pending') return <Icon name="clock" size="sm" />;
  if (status === 'delivered') return <Icon name="check" size="sm" />;
  if (status === 'read') return <Icon name="checkDouble" size="sm" />;
  return null;
}
export interface MessageListProps {
  messages: ChatMessage[];
  currentUserId: number;
  onRecall: (msgId: number) => void;
  onDelete: (msgId: number) => void;
  onForward: (msgId: number) => void;
  onEdit: (msgId: number, text: string) => void;
  onRetry: (msgId: number) => void;
}

export function MessageList({
  messages,
  currentUserId,
  onRecall,
  onDelete,
  onForward,
  onEdit,
  onRetry,
}: MessageListProps) {
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [atBottom, setAtBottom] = useState(true);

  // 初次加载与新消息：贴底。
  useLayoutEffect(() => {
    const node = scrollRef.current;
    if (!node || !atBottom) return;
    node.scrollTop = node.scrollHeight;
  }, [messages, atBottom]);

  useEffect(() => {
    const node = scrollRef.current;
    if (!node) return;
    const onScroll = () => {
      const distance = node.scrollHeight - node.scrollTop - node.clientHeight;
      setAtBottom(distance < 80);
    };
    node.addEventListener('scroll', onScroll, { passive: true });
    return () => node.removeEventListener('scroll', onScroll);
  }, []);

  const setReplyTo = useReplyStore((s) => s.setReplyTo);
  let lastDay = '';

  return (
    <div className="uf-msglist" ref={scrollRef} role="log" aria-live="polite" aria-relevant="additions">
      {!atBottom ? (
        <button
          type="button"
          className="uf-jump"
          onClick={() => {
            const node = scrollRef.current;
            if (node) node.scrollTop = node.scrollHeight;
          }}
        >
          <Icon name="back" size="sm" />
          回到最新
        </button>
      ) : null}

      {messages.length === 0 ? (
        <div className="uf-empty">
          <Icon name="chat" size="lg" className="uf-empty__icon" />
          <span className="uf-faint">还没有消息，说点什么吧。</span>
        </div>
      ) : null}

      {messages.map((message) => {
        const day = formatDay(message.sentAt);
        const showDay = day !== lastDay;
        lastDay = day;
        const mine = message.senderId === currentUserId;
        const reply = messages.find((m) => m.msgId === message.replyTo);

        return (
          <div key={`${message.msgId}`}>
            {showDay ? <div className="uf-daysep">{day}</div> : null}
            <div className={`uf-bubble-row ${mine ? 'uf-bubble-row--mine' : ''}`}>
              {!mine ? <span className="uf-bubble-row__spacer" /> : null}
              <div className={`uf-bubble ${mine ? 'uf-bubble--mine' : ''} ${message.tombstone ? 'uf-bubble--tomb' : ''}`}>
                {reply ? (
                  <div className="uf-quote">
                    <span className="uf-quote__text truncate">{reply.text || '[图片]'}</span>
                  </div>
                ) : null}

                {message.media?.length ? (
                  <div className="uf-media">
                    {message.media.map((media) => (
                      <div key={media.mediaId} className="uf-media__item">
                        <Icon
                          name={media.mtype === 1 ? 'image' : media.mtype === 2 ? 'mic' : 'file'}
                          size="lg"
                        />
                        <span className="uf-faint truncate">{media.name ?? media.mediaId}</span>
                      </div>
                    ))}
                  </div>
                ) : null}

                {message.text ? (
                  <span className="uf-bubble__text break-anywhere">{message.text}</span>
                ) : null}

                {message.tombstone ? <span className="uf-faint">消息已撤回</span> : null}

                <span className="uf-bubble__meta">
                  {formatTime(message.sentAt)}
                  {mine ? statusIcon(message.status) : null}
                </span>

                {mine && !message.tombstone ? (
                  <span className="uf-bubble__actions">
                    <IconButton icon="reply" label="回复" size="sm" onClick={() => setReplyTo(message)} />
                    <IconButton icon="edit" label="编辑" size="sm" onClick={() => onEdit(message.msgId, message.text)} />
                    <IconButton icon="forward" label="转发" size="sm" onClick={() => onForward(message.msgId)} />
                    <IconButton icon="trash" label="删除" size="sm" danger onClick={() => onDelete(message.msgId)} />
                    <IconButton icon="warning" label="撤回" size="sm" danger onClick={() => onRecall(message.msgId)} />
                  </span>
                ) : null}
                {mine && message.status === 'failed' ? (
                  <button type="button" className="uf-retry" onClick={() => onRetry(message.msgId)}>
                    发送失败，点此重试
                  </button>
                ) : null}
              </div>
              {mine ? <span className="uf-bubble-row__spacer" /> : null}
            </div>
          </div>
        );
      })}

    </div>
  );
}
