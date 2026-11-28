/** 输入区：文本、附件上传、发送、输入中上报、回复引用。 */

import { useEffect, useRef, useState } from 'react';
import { Icon } from '@/ui/primitives/Icon';
import { IconButton, Spinner } from '@/ui/primitives/controls';
import { useMsgStore, nextLocalId } from '@/core/store/msg';
import { useConvStore } from '@/core/store/conv';
import { useUiStore } from '@/core/store/ui';
import { useAuthStore } from '@/core/store/auth';
import { sendMessage, sendTyping } from '@/core/store/bridge';
import { precheckFile, uploadMedia } from '@/core/domain/media';
import type { MediaItem } from '@/core/domain/types';
import { useReplyStore } from './replyStore';

export function Composer({ convId }: { convId: number }) {
  const [text, setText] = useState('');
  const [pendingMedia, setPendingMedia] = useState<MediaItem[]>([]);
  const [uploading, setUploading] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const fileRef = useRef<HTMLInputElement | null>(null);
  const pushToast = useUiStore((s) => s.pushToast);
  const replyTo = useReplyStore((s) => s.replyTo);
  const setReplyTo = useReplyStore((s) => s.setReplyTo);
  const conv = useConvStore((s) => s.conversations[convId]);
  const userId = useAuthStore((s) => s.session?.user_id ?? 0);

  // 自适应高度
  useEffect(() => {
    const node = textareaRef.current;
    if (!node) return;
    node.style.height = 'auto';
    node.style.height = `${Math.min(node.scrollHeight, 160)}px`;
  }, [text]);

  const submit = () => {
    const body = text.trim();
    if (!body && pendingMedia.length === 0) return;
    const clientMsgId = `c-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    const mentions = Array.from(body.matchAll(/@(\d{1,20})/g)).map((m) => Number(m[1]));
    const local = {
      msgId: nextLocalId(),
      convId,
      seq: 0,
      senderId: userId,
      text: body,
      media: pendingMedia.length ? pendingMedia : undefined,
      replyTo: replyTo?.msgId,
      mentions: mentions.length ? mentions : undefined,
      sentAt: Date.now(),
      status: 'pending' as const,
      clientMsgId,
    };

    useMsgStore.getState().appendLocal(local);
    const ok = sendMessage({
      convId,
      text: body,
      media: pendingMedia,
      replyTo: replyTo?.msgId,
      mentions: mentions.length ? mentions : undefined,
      clientMsgId,
    });
    if (!ok) {
      useMsgStore.getState().applyAck({ clientMsgId, msgId: 0, seq: 0, status: 1 });
      pushToast('网络恢复后重试', 'error');
    }
    setText('');
    setPendingMedia([]);
    setReplyTo(null);
  };

  const onPickFiles = async (files: FileList | null) => {
    if (!files || files.length === 0) return;
    const file = files[0];
    if (!file) return;
    const problem = precheckFile(file);
    if (problem) {
      pushToast(problem, 'warn');
      return;
    }
    setUploading(true);
    try {
      const media = await uploadMedia(file);
      setPendingMedia((prev) => [...prev, media]);
    } catch (error) {
      pushToast(error instanceof Error ? error.message : '上传失败', 'error');
    } finally {
      setUploading(false);
      if (fileRef.current) fileRef.current.value = '';
    }
  };

  const insertMention = (userId: number, name: string) => {
    setText((prev) => `${prev}@${name} `);
    void userId;
  };

  return (
    <div className="uf-composer">
      {replyTo ? (
        <div className="uf-composer__reply">
          <Icon name="reply" size="sm" />
          <span className="truncate uf-grow">{replyTo.text || '（媒体消息）'}</span>
          <IconButton icon="close" label="取消回复" size="sm" onClick={() => setReplyTo(null)} />
        </div>
      ) : null}

      {pendingMedia.length > 0 ? (
        <div className="uf-composer__attachments">
          {pendingMedia.map((media) => (
            <span key={media.mediaId} className="uf-chip">
              <Icon name={media.mtype === 1 ? 'image' : media.mtype === 2 ? 'mic' : 'file'} size="sm" />
              <span className="truncate">{media.name ?? media.mediaId}</span>
            </span>
          ))}
        </div>
      ) : null}

      <div className="uf-composer__row">
        <input
          ref={fileRef}
          type="file"
          accept="image/jpeg,image/png,image/webp,image/gif,audio/ogg,audio/mpeg"
          hidden
          onChange={(event) => void onPickFiles(event.target.files)}
        />
        <IconButton
          icon="attach"
          label="添加图片或文件"
          onClick={() => fileRef.current?.click()}
          disabled={uploading}
        />
        <textarea
          ref={textareaRef}
          className="uf-textarea uf-composer__input"
          rows={1}
          value={text}
          placeholder="输入消息"
          aria-label="消息输入框"
          onChange={(event) => {
            setText(event.target.value);
            sendTyping(convId, true);
          }}
          onKeyDown={(event) => {
            if (event.key === 'Enter' && !event.shiftKey) {
              event.preventDefault();
              submit();
            }
          }}
        />
        {uploading ? <Spinner /> : null}
        <IconButton
          icon="send"
          label="发送"
          className="uf-iconbtn--active"
          onClick={submit}
          disabled={!text.trim() && pendingMedia.length === 0}
        />
      </div>

      {conv?.members?.length ? (
        <div className="uf-composer__mentions">
          {conv.members.slice(0, 6).map((member) => (
            <button
              key={member.userId}
              type="button"
              className="uf-chip uf-chip--btn"
              onClick={() => insertMention(member.userId, String(member.userId))}
            >
              @{member.userId}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}
