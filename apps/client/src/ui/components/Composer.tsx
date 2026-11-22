import { useEffect, useRef, useState, type ChangeEvent, type ReactNode } from 'react';
import { getAppServices } from '../../app/services';
import { getState, setDraft } from '../../state/chat';
import type { ChatMessage } from '../../domain/types';
import { t } from '../i18n';
import { onComposerKey } from './MessageList';
import { toast } from './Toast';

interface ReplyTarget { id: string; text: string; }

export function Composer({ convId }: { convId: number }): ReactNode {
  const services = getAppServices();
  const [text, setText] = useState(() => getState().draft[convId] || '');
  const [reply, setReply] = useState<ReplyTarget | null>(null);
  const fileRef = useRef<HTMLInputElement>(null);
  const mediaRef = useRef<string[]>([]);

  useEffect(() => {
    const handler = (event: Event) => {
      const message = (event as CustomEvent<{ message: ChatMessage }>).detail?.message;
      if (message) setReply({ id: message.id, text: message.text });
    };
    window.addEventListener('ufot:reply', handler);
    return () => window.removeEventListener('ufot:reply', handler);
  }, []);

  function send(): void {
    const value = text.trim();
    if (!value && !mediaRef.current.length) return;
    services.sendMessage({
      convId,
      clientMsgId: `web-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
      text: value,
      replyTo: reply?.id,
      mentions: [],
      media: mediaRef.current,
    });
    setText('');
    setDraft(convId, '');
    setReply(null);
    mediaRef.current = [];
  }

  async function onFile(event: ChangeEvent<HTMLInputElement>): Promise<void> {
    const files = Array.from(event.target.files || []);
    if (!files.length) return;
    try {
      for (const file of files) mediaRef.current.push(await services.upload(file));
      toast(t('settings.saved'));
    } catch {
      toast(t('error.generic'));
    }
    event.target.value = '';
  }

  return (
    <div className="composer">
      {reply ? (
        <div className="reply-strip">
          <span>{t('chat.reply')}: {reply.text.slice(0, 80)}</span>
          <button className="btn small" type="button" onClick={() => setReply(null)}>{t('chat.cancel') || t('common.cancel')}</button>
        </div>
      ) : null}
      <div className="composer-row">
        <input ref={fileRef} type="file" multiple hidden onChange={(event) => void onFile(event)} />
        <button className="icon-btn" type="button" aria-label={t('chat.attach')} onClick={() => fileRef.current?.click()}>+</button>
        <textarea
          className="textarea"
          rows={2}
          value={text}
          placeholder={t('chat.placeholder')}
          onChange={(event) => { setText(event.target.value); setDraft(convId, event.target.value); }}
          onKeyDown={(event) => onComposerKey(event, send)}
        />
        <button className="btn primary" type="button" onClick={send}>{t('chat.send')}</button>
      </div>
    </div>
  );
}
