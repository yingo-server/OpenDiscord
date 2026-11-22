/**
 * Avatar —— 基于微软官方 @fluentui/react-components v9 的 Avatar + PresenceBadge。
 * 保留本项目 API（name/src/size/userId/presence）。
 */
import {
  Avatar as FluentAvatar,
  PresenceBadge,
} from '@fluentui/react-components';
import type { PresenceBadgeProps } from '@fluentui/react-components';

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

const FSIZE = {
  xs: 20, sm: 28, md: 40, lg: 56, xl: 72,
} as const;

const FSTATUS: Record<Presence, PresenceBadgeProps['status']> = {
  online: 'available',
  away: 'away',
  offline: 'offline',
};

export function Avatar({ name, src, size = 'md', userId = 0, presence, className = '' }: AvatarProps) {
  return (
    <span className={`relative inline-flex shrink-0 ${className}`}>
      <FluentAvatar
        name={name || String(userId)}
        image={{ src: src || undefined }}
        size={FSIZE[size]}
        aria-label={`${name}的头像`}
      />
      {presence && (
        <PresenceBadge
          status={FSTATUS[presence]}
          size="small"
          className="absolute bottom-0 end-0 ring-2 ring-surface"
        />
      )}
    </span>
  );
}
