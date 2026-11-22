import type { KeyboardEvent as ReactKeyboardEvent } from 'react';
import { useEffect, useRef, type ReactNode } from 'react';
import { useSyncExternalStore } from 'react';
import { getState, subscribe, type ChatState } from '../../state/chat';
import { messagesFor, getAppServices } from '../../app/services';
import type { ChatMessage } from '../../domain/types';
import { t } from '../i18n';
import { cn, formatTime, initials, safeUrl } from '../lib/format';

function AttachmentView({ message }: { message: ChatMessage }): ReactNode {
  if (!message.media.length) return null;
  const base = getAppServices().http.endpoint;
  return (
    <div className="media-grid">
      {message.media.map((item, index) => {
        const id = item.id || item.media_id || item.mediaId || '';
        const url = safeUrl(item.url || '') || (id ? `${base}/api/media/${encodeURIComponent(id)}` : '');
        if (item.kind === 'image' && url) {
          return <div className="media-item" key={id || index}><img src={url} alt={item.name || 'image'} loading="lazy" /></div>;
        }
        if (item.kind === 'audio' && url) {
          const bars = Array.isArray(item.waveform) && item.waveform.length ? item.waveform.slice(0, 24) : Array.from({ length: 24 }, (_, i) => 30 + ((i * 17) % 60));
          return (
            <div className="media-item" key={id || index}>
              <audio controls preload="none" src={url} style={{ width: '100%' }} />
              <div className="waveform" aria-hidden="true">{bars.map((h, i) => <i key={i} style={{ height: `${Math.max(10, Math.min(100, Number(h)))}%` }} />)}</div>
            </div>
          );
        }
        return <div className="media-item" key={id || index}>{url ? <a className="file" href={url} target="_blank" rel="noopener noreferrer">↓ {item.name || t('chat.attach')}</a> : <div className="file">{t('chat.media.unsupported')}</div>}</div>;
      })}
    </div>
  );
}

function MessageActions({ message }: { message: ChatMessage }): ReactNode {
  const services = getAppServices();
  return (
    <div className="msg-actions">
      <button type="button" onClick={() => void navigator.clipboard?.writeText(message.text)}>{t('chat.copy')}</button>
      <button type="button" onClick={() => window.dispatchEvent(new CustomEvent('ufot:reply', { detail: { message } }))}>{t('chat.reply')}</button>
      {message.pending || message.status === 'failed'
        ? <button type="button" onClick={() => services.retryMessage(message.convId, message.id)}>{t('chat.retry')}</button>
        : null}
      <button type="button" onClick={() => {
        const text = window.prompt(t('chat.edit'), message.text);
        if (text !== null && text.trim()) services.editMessage(message.convId, message.id, text.trim());
      }}>{t('chat.edit')}</button>
      <button type="button" onClick={() => {
        const target = window.prompt(t('chat.forward'));
        if (target && Number(target) > 0) services.forwardMessage(message.convId, message.id, Number(target));
      }}>{t('chat.forward')}</button>
      <button type="button" onClick={() => services.recallMessage(message.convId, message.id)}>{t('chat.recall')}</button>
      <button type="button" onClick={() => services.deleteMessage(message.convId, message.id)}>{t('chat.delete')}</button>
    </div>
  );
}

function MessageRow({ message, mine, senderName, prevSeq, latest }: { message: ChatMessage; mine: boolean; senderName: string; prevSeq: number; latest: boolean }): ReactNode {
  const services = getAppServices();
  useEffect(() => {
    if (latest && message.seq > 0) services.sendRead(message.convId, message.seq);
  }, [latest, message.seq, message.convId, services]);
  const status = message.pending ? '…' : message.status === 'failed' ? '!' : message.status === 'read' || message.status === 'delivered' ? '✓✓' : message.status === 'sent' ? '✓' : '';
  return (
    <div className={cn('msg-row', mine && 'mine')} data-seq={message.seq}>
      <div className="avatar" aria-hidden="true">{initials(senderName)}</div>
      <div className="msg-body">
        <div className="msg-meta">
          <span>{mine ? t('settings.profile') : senderName}</span>
          <span>{formatTime(message.createdAt)}</span>
          {message.edited ? <span>{t('chat.edited')}</span> : null}
          {mine ? <span aria-label={message.status}>{status}</span> : null}
          {prevSeq > 0 && message.seq > prevSeq + 1
            ? <button type="button" className="btn small" onClick={() => services.syncConversation(message.convId, prevSeq)}>{t('chat.retry')}</button>
            : null}
        </div>
        <div className={cn('bubble', message.status === 'failed' && 'failed', message.deleted && 'deleted', message.recalled && 'recalled')}>
          {message.deleted ? t('chat.deleted') : message.recalled ? t('chat.recalled') : message.text || '—'}
          <AttachmentView message={message} />
        </div>
        <MessageActions message={message} />
      </div>
    </div>
  );
}

export function MessageList({ convId, state }: { convId: number; state: ChatState }): ReactNode {
  const messages = messagesFor(convId);
  const scrollRef = useRef<HTMLDivElement>(null);
  const lastId = messages.length ? messages[messages.length - 1].id : '';
  useEffect(() => {
    const node = scrollRef.current;
    if (node) node.scrollTop = node.scrollHeight;
  }, [lastId]);
  if (!messages.length) return <div className="empty"><div><h3>{t('chat.empty')}</h3></div></div>;
  return (
    <div className="message-scroll" ref={scrollRef}>
      <div className="message-list">
        {messages.map((message, index) => (
          <MessageRow
            key={message.id}
            message={message}
            mine={message.senderId === (getAppServices().session()?.user_id || 0)}
            senderName={message.senderId ? String(message.senderId) : t('settings.profile')}
            prevSeq={index > 0 ? messages[index - 1].seq : 0}
            latest={index === messages.length - 1}
          />
        ))}
        {state.typing[convId]?.length ? <div className="typing-line">{t('chat.typing')}</div> : null}
      </div>
    </div>
  );
}

export function useChatState(): ChatState {
  return useSyncExternalStore(subscribe, getState, getState);
}

export function onComposerKey(event: ReactKeyboardEvent<HTMLTextAreaElement>, onSend: () => void): void {
  if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing) {
    event.preventDefault();
    onSend();
  }
}
