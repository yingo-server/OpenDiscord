/**
 * 文件渲染器（§4.4）：按扩展名 5 类图标 + 文件名截断 + 大小/进度（等宽数字）。
 * 接收中的进度条 4px 高、主色填充；完成后消失。
 */
import type { Media } from '../../net/proto/payload';
import { t } from '../i18n';
import { DownloadIcon, FileIcon, ImageIcon, MicIcon } from '../components/icons';
import { formatBytes } from '../utils/format';
import { resolveMediaUrl } from '../../net/media';
import { UnknownMessage } from './unknown';
import type { RendererProps } from './registry';

type FileCategory = 'doc' | 'sheet' | 'slide' | 'archive' | 'other';

const CATEGORY_STYLE: Record<FileCategory, string> = {
  doc: 'bg-accent/10 text-accent',
  sheet: 'bg-success-100 text-success-700',
  slide: 'bg-warning-100 text-warning-700',
  archive: 'bg-info-100 text-info-700',
  other: 'bg-surface-2 text-ink-secondary',
};

export function FileMessage({ message }: RendererProps) {
  const media = message.payload.media?.[0];
  if (!media) return <UnknownMessage message={message} />;

  const category = categoryOf(media.name ?? '');
  const uploading = media.status === 'uploading' || media.status === 'pending';
  const expired = media.status === 'expired';
  const progress = Math.round(
    Math.min(100, Math.max(0, Number(message.payload.progress ?? (uploading ? 64 : 0)))),
  );

  const downloadUrl = resolveMediaUrl(media);

  const body = (
    <span className="flex items-center gap-3 text-start">
      <span
        className={`inline-flex h-10 w-10 shrink-0 items-center justify-center rounded-md ${CATEGORY_STYLE[category]}`}
      >
        {iconFor(media, category)}
      </span>
      <span className="min-w-0 flex-1">
        <span className="block truncate text-body text-current">{media.name || t('chat.file')}</span>
        <span className="nums mt-0.5 block text-caption opacity-70">
          {formatBytes(media.size ?? 0)}
          {expired ? ` · ${t('media.expired')}` : uploading ? '' : ` · ${t('media.download')}`}
        </span>
      </span>
      {!uploading && !expired ? <DownloadIcon size={18} className="shrink-0 opacity-70" /> : null}
    </span>
  );

  return (
    <span className="block" style={{ minWidth: 220, maxWidth: 300 }}>
      {downloadUrl && !uploading && !expired ? (
        <a
          href={downloadUrl}
          download={media.name}
          target="_blank"
          rel="noreferrer"
          className="block rounded-sm transition-opacity duration-micro hover:opacity-80"
        >
          {body}
        </a>
      ) : (
        <span className="block">{body}</span>
      )}
      {uploading ? (
        <span className="mt-2 block h-1 w-full overflow-hidden rounded-full bg-ink/[0.12]">
          <span
            className="block h-full bg-accent transition-[width] duration-normal"
            style={{ width: `${progress}%` }}
          />
        </span>
      ) : null}
    </span>
  );
}

function categoryOf(name: string): FileCategory {
  const ext = name.split('.').pop()?.toLowerCase() ?? '';
  if (['pdf', 'doc', 'docx', 'txt', 'md', 'rtf'].includes(ext)) return 'doc';
  if (['xls', 'xlsx', 'csv', 'numbers'].includes(ext)) return 'sheet';
  if (['ppt', 'pptx', 'key'].includes(ext)) return 'slide';
  if (['zip', 'rar', '7z', 'tar', 'gz', 'bz2'].includes(ext)) return 'archive';
  return 'other';
}

function iconFor(media: Media, category: FileCategory) {
  if (media.type === 'image') return <ImageIcon size={20} />;
  if (media.type === 'audio') return <MicIcon size={20} />;
  return <FileIcon size={20} />;
}
