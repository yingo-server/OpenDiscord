/**
 * Avatar（Fluent / WinUI 3）：PersonPicture 风格——首字符 + ID hash 色板兜底，
 * 头像裁切为圆形，presence 圆点带表面色描边，与 Fluent 状态点一致。
 */
import { useState } from 'react';

export type AvatarSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl';
export type Presence = 'online' | 'away' | 'offline';

export interface AvatarProps {
  name: string;
  src?: string;
  size?: AvatarSize;
  userId?: number;
  presence?: Presence;
  className?: string;
}

const SIZE_CLASS: Record<AvatarSize, string> = {
  xs: 'h-avatar-xs w-avatar-xs text-caption',
  sm: 'h-avatar-sm w-avatar-sm text-body-sm',
  md: 'h-avatar-md w-avatar-md text-body',
  lg: 'h-avatar-lg w-avatar-lg text-title',
  xl: 'h-avatar-xl w-avatar-xl text-display',
};

const DOT_CLASS: Record<AvatarSize, string> = {
  xs: 'h-2 w-2',
  sm: 'h-2.5 w-2.5',
  md: 'h-3 w-3',
  lg: 'h-3.5 w-3.5',
  xl: 'h-4 w-4',
};

const FALLBACK_BG = [
  'bg-primary-500',
  'bg-success-600',
  'bg-warning-600',
  'bg-info-600',
  'bg-danger-500',
  'bg-primary-700',
  'bg-info-500',
  'bg-success-500',
];

export function Avatar({ name, src, size = 'md', userId = 0, presence, className = '' }: AvatarProps) {
  const [broken, setBroken] = useState(false);
  const initial = firstChar(name);
  const palette = FALLBACK_BG[hash(name || String(userId)) % FALLBACK_BG.length];
  const showImage = Boolean(src) && !broken;

  return (
    <span className={`relative inline-flex shrink-0 ${className}`}>
      {showImage ? (
        <img
          src={src}
          alt={`${name}的头像`}
          loading="lazy"
          decoding="async"
          onError={() => setBroken(true)}
          className={`${SIZE_CLASS[size]} rounded-full object-cover ring-1 ring-line`}
        />
      ) : (
        <span
          role="img"
          aria-label={`${name}的头像`}
          className={`${SIZE_CLASS[size]} ${palette} inline-flex items-center justify-center rounded-full font-semibold text-ink-inverse ring-1 ring-line`}
        >
          {initial}
        </span>
      )}
      {presence && (
        <span
          title={presence}
          aria-label={presence}
          className={[
            DOT_CLASS[size],
            'absolute bottom-0 end-0 rounded-full ring-2 ring-surface',
            presence === 'online'
              ? 'bg-success-500'
              : presence === 'away'
                ? 'bg-warning-500'
                : 'bg-transparent ring-2 ring-line',
          ].join(' ')}
        />
      )}
    </span>
  );
}

function firstChar(name: string): string {
  const trimmed = (name || '?').trim();
  return [...trimmed][0]?.toUpperCase() ?? '?';
}

function hash(value: string): number {
  let h = 0;
  for (let i = 0; i < value.length; i += 1) h = (h * 31 + value.charCodeAt(i)) | 0;
  return Math.abs(h);
}
