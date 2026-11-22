/**
 * 图片渲染器（§4.2）：三级渐进
 *   1) 占位：后端 thumb_inline（低清）/ 无缩略图时按 mediaId hash 生成纯色渐变；
 *   2) 缩略图：等高宽比容器，首帧即占位（防 CLS）；
 *   3) 原图：点击后全屏查看器加载。
 * 宽度按 3 档收敛（160/240/320），保证虚拟列表测量稳定。
 */
import { useEffect, useState } from 'react';
import { createPortal } from 'react-dom';
import type { Media } from '../../net/proto/payload';
import { t } from '../i18n';
import { CloseIcon, ImageIcon } from '../components/icons';
import { formatBytes } from '../utils/format';
import { resolveMediaUrl } from '../../net/media';
import { UnknownMessage } from './unknown';
import type { RendererProps } from './registry';

export function ImageMessage({ message }: RendererProps) {
  const media = message.payload.media?.[0];
  const [viewerOpen, setViewerOpen] = useState(false);
  const [loaded, setLoaded] = useState(false);

  if (!media) return <UnknownMessage message={message} />;

  const width = media.width || 800;
  const height = media.height || 600;
  const bucket = width >= 1200 ? 320 : width >= 600 ? 240 : 160;
  const src = resolveMediaUrl(media) || thumbSource(media.thumb);
  const thumbSrc = thumbSource(media.thumb);
  const hue = hueOf(media.mediaId || String(message.msgId));
  const uploading = media.status === 'uploading' || media.status === 'pending';
  const progress = Math.round(
    Math.min(100, Math.max(0, Number(message.payload.progress ?? (uploading ? 64 : 0)))),
  );

  return (
    <>
      <button
        type="button"
        onClick={() => src && setViewerOpen(true)}
        className="relative block max-w-full overflow-hidden rounded-lg"
        style={{ width: bucket, aspectRatio: `${width} / ${height}`, maxWidth: '100%' }}
        aria-label={t('media.imageAlt', { name: media.name ?? '' })}
      >
        <span
          aria-hidden="true"
          className="absolute inset-0"
          style={{
            background: `linear-gradient(135deg, hsl(${hue} 68% 62%), hsl(${(hue + 42) % 360} 62% 42%))`,
          }}
        />
        {thumbSrc && !loaded ? (
          <img
            src={thumbSrc}
            alt=""
            aria-hidden="true"
            className="absolute inset-0 h-full w-full scale-105 object-cover blur-xl"
          />
        ) : null}
        {src ? (
          <img
            src={src}
            alt={media.name ?? ''}
            loading="lazy"
            decoding="async"
            onLoad={() => setLoaded(true)}
            className={`absolute inset-0 h-full w-full object-cover transition-opacity duration-slow ${
              loaded ? 'opacity-100' : 'opacity-0'
            }`}
          />
        ) : (
          <span className="absolute inset-0 flex flex-col items-center justify-center gap-2 text-ink-inverse/80">
            <ImageIcon size={28} />
            <span className="max-w-[90%] truncate px-2 text-caption">{media.name}</span>
          </span>
        )}
        {uploading ? (
          <span className="absolute inset-x-0 bottom-0 block">
            <span className="block h-1 w-full bg-ink/20">
              <span
                className="block h-full bg-accent transition-[width] duration-normal"
                style={{ width: `${progress}%` }}
              />
            </span>
            <span className="block bg-scrim/40 px-2 py-0.5 text-end text-caption text-ink-inverse">
              {t('media.uploading', { percent: progress })}
            </span>
          </span>
        ) : null}
      </button>

      {viewerOpen && src
        ? createPortal(
            <ImageViewer
              src={src}
              name={media.name}
              size={media.size}
              onClose={() => setViewerOpen(false)}
            />,
            document.body,
          )
        : null}
    </>
  );
}

function ImageViewer({
  src,
  name,
  size,
  onClose,
}: {
  src: string;
  name?: string;
  size?: number;
  onClose: () => void;
}) {
  useEffect(() => {
    const handler = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', handler);
    const previous = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.removeEventListener('keydown', handler);
      document.body.style.overflow = previous;
    };
  }, [onClose]);

  return (
    <div
      className="fixed inset-0 z-modal flex animate-fade-in items-center justify-center bg-scrim/70"
      role="dialog"
      aria-modal="true"
      aria-label={t('media.viewer.close')}
      onClick={onClose}
    >
      <button
        type="button"
        onClick={onClose}
        aria-label={t('common.close')}
        className="pt-safe absolute end-4 top-4 inline-flex h-10 w-10 items-center justify-center rounded-full bg-ink/50 text-ink-inverse hover:bg-ink/70"
      >
        <CloseIcon size={20} />
      </button>
      <img
        src={src}
        alt={name ?? ''}
        onClick={(event) => event.stopPropagation()}
        className="max-h-[86dvh] max-w-[94vw] rounded-lg object-contain shadow-lg"
      />
      <div className="absolute inset-x-0 bottom-0 bg-scrim/50 px-4 py-3 text-center text-body-sm text-ink-inverse">
        {t('media.viewer.info', { name: name ?? '', size: formatBytes(size ?? 0) })}
      </div>
    </div>
  );
}

function thumbSource(thumb?: string): string | undefined {
  if (!thumb) return undefined;
  if (thumb.startsWith('data:') || thumb.startsWith('http') || thumb.startsWith('blob:')) return thumb;
  return `data:image/jpeg;base64,${thumb}`;
}

function hueOf(value: string): number {
  let h = 0;
  for (let i = 0; i < value.length; i += 1) h = (h * 31 + value.charCodeAt(i)) | 0;
  return Math.abs(h) % 360;
}
