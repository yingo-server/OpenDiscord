/**
 * Composer（§3.2）：自适应高度、草稿持久化、Enter 发送（桌面）、
 * 500 字计数（90% 出现）、回复预览、离线仍可入队；📎 媒体上传走 mediasvc。
 */
import { useEffect, useRef, useState } from 'react';
import { useConvStore } from '../../../core/stores/conv-store';
import { useSessionStore } from '../../../core/stores/session-store';
import { useConnStore } from '../../../core/stores/conn-store';
import { selectCanSendMessage } from '../../../core/selectors';
import type { ChatMessage } from '../../../core/types';
import { getAppServices } from '../../services/app-services';
import { t } from '../../i18n';
import { Textarea } from '@fluentui/react-components';
import { AttachIcon, CloseIcon, SendIcon } from '../../components/icons';
import { IconButton } from '../../components/IconButton';
import { toast } from '../../components/Toast';
import {
  MediaUploadError,
  UPLOAD_ACCEPT,
  UPLOAD_LIMITS,
  classifyUpload,
  mtypeToMediaType,
  uploadMedia,
  type UploadExtras,
} from '../../../net/media';
import { makeThumbnail, makeWaveform } from '../../../net/media-encode';
import type { Media } from '../../../net/proto/payload';

export interface ComposerProps {
  convId: number;
  replyTo?: ChatMessage | null;
  replySenderName?: string;
  onCancelReply?: () => void;
  onSent?: () => void;
}

export function Composer({ convId, replyTo, replySenderName, onCancelReply, onSent }: ComposerProps) {
  const text = useConvStore((state) => state.drafts[convId] ?? '');
  const setDraft = useConvStore((state) => state.setDraft);
  const canSend = useSessionStore(selectCanSendMessage);
  const guest = useSessionStore((state) => state.status === 'guest');
  const maxLength = useSessionStore((state) => Number(state.flags['chat.max_length'] ?? 500) || 500);
  const textareaRef = useRef<HTMLTextAreaElement | null>(null);
  const fileInputRef = useRef<HTMLInputElement | null>(null);
  const [uploading, setUploading] = useState(false);
  const [uploadRatio, setUploadRatio] = useState(0);

  useEffect(() => {
    const element = textareaRef.current;
    if (!element) return;
    element.style.height = 'auto';
    const max = Math.min(window.innerHeight * 0.4, 220);
    element.style.height = `${Math.min(element.scrollHeight, max)}px`;
  }, [text]);

  const send = () => {
    const value = text.trim();
    if (!value || !canSend) return;
    const outbox = getAppServices().outbox;
    if (!outbox) return;
    void outbox.enqueue({
      convId,
      text: value,
      replyTo: replyTo?.msgId || undefined,
    });
    setDraft(convId, '');
    onCancelReply?.();
    onSent?.();
    if (useConnStore.getState().state !== 'online') {
      toast.info(t('toast.queued'));
    }
    textareaRef.current?.focus();
  };

  const attach = () => {
    if (guest) {
      toast.info(t('toast.mediaUnsupported'));
      return;
    }
    if (uploading) {
      toast.info(t('toast.mediaUploading'));
      return;
    }
    fileInputRef.current?.click();
  };

  const onFilePicked = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = ''; // 允许重选同一文件
    if (!file || !canSend) return;
    void pickAndSend(file);
  };

  const pickAndSend = async (file: File) => {
    const outbox = getAppServices().outbox;
    if (!outbox) return;
    setUploading(true);
    setUploadRatio(0);
    try {
      const extras = await buildThumbExtras(file);
      const result = await uploadMedia(file, (ratio) => setUploadRatio(ratio), extras);
      const media = await buildMediaPayload(result, file, extras);
      outbox.enqueue({ convId, text: '', media: [media], replyTo: replyTo?.msgId || undefined });
      setDraft(convId, '');
      onCancelReply?.();
      onSent?.();
      if (useConnStore.getState().state !== 'online') {
        toast.info(t('toast.queued'));
      }
    } catch (error) {
      toast.danger(t('toast.mediaUploadFailed', { reason: reasonOf(error) }));
    } finally {
      setUploading(false);
      setUploadRatio(0);
    }
  };

  const remaining = maxLength - text.length;
  const showCounter = text.length >= maxLength * 0.9;

  return (
    <div className="shrink-0 border-t border-line bg-surface pb-safe">
      {replyTo ? (
        <div className="mx-3 mt-2 flex items-start gap-2 rounded-md border-s-[3px] border-s-accent bg-accent/[0.06] px-2.5 py-1.5">
          <div className="min-w-0 flex-1">
            <p className="text-caption text-accent">{t('chat.replyTo', { name: replySenderName ?? '' })}</p>
            <p className="truncate text-caption text-ink-tertiary">{previewOfMessage(replyTo)}</p>
          </div>
          <IconButton
            label={t('chat.cancelReply')}
            icon={<CloseIcon size={14} />}
            onClick={onCancelReply}
          />
        </div>
      ) : null}

      <div className="flex items-end gap-2 px-3 py-2">
        <IconButton
          label={t('chat.attach')}
          onClick={attach}
          disabled={uploading}
          icon={<AttachIcon size={22} />}
        />
        <input
          ref={fileInputRef}
          type="file"
          accept={UPLOAD_ACCEPT}
          className="hidden"
          onChange={onFilePicked}
          aria-hidden="true"
          tabIndex={-1}
        />

        <div className="relative min-w-0 flex-1">
          {uploading ? (
            <div className="mb-1 h-1 w-full overflow-hidden rounded-full bg-ink/15">
              <div
                className="h-full bg-primary-500 transition-[width] duration-normal"
                style={{ width: `${Math.round(uploadRatio * 100)}%` }}
              />
            </div>
          ) : null}
          <Textarea
            ref={textareaRef}
            value={text}
            rows={1}
            maxLength={maxLength}
            resize="none"
            onChange={(_, data) => setDraft(convId, data.value)}
            onKeyDown={(event) => {
              const isMobile = window.matchMedia('(max-width: 767px)').matches;
              if (event.key === 'Enter' && !event.shiftKey && !isMobile) {
                event.preventDefault();
                send();
              }
            }}
            placeholder={t('chat.placeholder')}
            aria-label={t('chat.placeholder')}
            className="max-h-[40vh] min-h-[44px] w-full overflow-y-auto ps-3 pe-14 py-2.5 text-body"
          />
          {showCounter ? (
            <span
              className={`nums pointer-events-none absolute bottom-1.5 end-2 text-caption ${
                remaining < 0 ? 'text-danger-500' : 'text-ink-tertiary'
              }`}
            >
              {text.length}/{maxLength}
            </span>
          ) : null}
        </div>

        <IconButton
          label={t('chat.send')}
          icon={<SendIcon size={20} />}
          onClick={send}
          disabled={!text.trim() || !canSend}
          className="h-11 w-11 shrink-0"
        />
      </div>
    </div>
  );
}

function previewOfMessage(message: ChatMessage): string {
  const text = typeof message.payload.text === 'string' ? message.payload.text : '';
  if (text) return text;
  const type = message.payload.media?.[0]?.type;
  if (type === 'image') return t('chat.image');
  if (type === 'audio') return t('chat.audio');
  if (type === 'video') return t('chat.video');
  if (type === 'file') return t('chat.file');
  return t('chat.unsupported');
}

/**
 * 按文件类型生成缩略图 / 波形附加字段（失败不阻塞上传）。
 * 图片 → thumb_data；音频 → waveform（24 段 0-100）。
 */
async function buildThumbExtras(file: File): Promise<UploadExtras> {
  const extras: UploadExtras = {};
  let mtype: number;
  try {
    ({ mtype } = await classifyUpload(file));
  } catch {
    // 分类失败交给 uploadMedia 统一报错，这里不重复处理
    return extras;
  }
  if (mtype === 1) {
    const thumb = await makeThumbnail(file);
    if (thumb) extras.thumbData = thumb;
  } else if (mtype === 2) {
    extras.waveform = await makeWaveform(file);
  }
  return extras;
}

/** 上传结果 → wire Media（media_id/mtype/name/size/mime/width/height…）。 */
async function buildMediaPayload(
  result: Awaited<ReturnType<typeof uploadMedia>>,
  file: File,
  extras?: UploadExtras,
): Promise<Media> {
  const type = mtypeToMediaType(result.mtype);
  const media: Media = {
    mediaId: result.mediaId,
    type,
    name: file.name,
    size: result.size || file.size,
    mime: result.mime || file.type || undefined,
    status: 'ready',
  };
  if (type === 'image') {
    if (extras?.thumbData) media.thumb = extras.thumbData;
    // 图片宽高从 Image 对象取（读不到时渲染器自行兜底 800x600）
    const dims = await imageSize(file);
    if (dims) {
      media.width = dims.width;
      media.height = dims.height;
    }
  } else if (type === 'audio') {
    // 波形在 wire 上传为 0-100 整数；展示层 Media.waveform 归一化到 0-1
    if (extras?.waveform?.length) media.waveform = extras.waveform.map((value) => value / 100);
  }
  return media;
}

function imageSize(file: File): Promise<{ width: number; height: number } | null> {
  return new Promise((resolve) => {
    const objectUrl = URL.createObjectURL(file);
    const image = new Image();
    image.onload = () => {
      URL.revokeObjectURL(objectUrl);
      resolve({ width: image.naturalWidth, height: image.naturalHeight });
    };
    image.onerror = () => {
      URL.revokeObjectURL(objectUrl);
      resolve(null);
    };
    image.src = objectUrl;
  });
}

/** MediaUploadError.code → i18n key；未知错误给通用文案。 */
function reasonOf(error: unknown): string {
  if (error instanceof MediaUploadError) {
    switch (error.code) {
      case 'FILE_TOO_LARGE':
        return t('toast.mediaTooLarge', {
          image: formatMb(UPLOAD_LIMITS.image),
          audio: formatMb(UPLOAD_LIMITS.audio),
          file: formatMb(UPLOAD_LIMITS.file),
        });
      case 'UNSUPPORTED_TYPE':
        return t('toast.mediaUnsupportedType');
      case 'RATE_LIMITED':
        return t('toast.mediaRateLimited', { count: 50 });
      case 'UNAUTHORIZED':
        return t('toast.mediaUnauthorized');
      default:
        break;
    }
  }
  return t('toast.mediaNetworkError');
}

function formatMb(bytes: number): string {
  return String(Math.round(bytes / (1024 * 1024)));
}
