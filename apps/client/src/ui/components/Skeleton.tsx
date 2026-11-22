/**
 * Skeleton（Fluent / WinUI 3）：形状与真实内容一一对应；300ms 延迟 + 最少 300ms（§5.3）。
 * 骨架底色由 --bg-surface-2 / --bg-elevated 令牌驱动，暗色主题自动适配。
 */
import { useSkeletonVisible } from '../hooks/useDelayedFlag';

export interface SkeletonProps {
  className?: string;
}

export function Skeleton({ className = '' }: SkeletonProps) {
  return <div className={`skeleton rounded-md ${className}`} aria-hidden="true" />;
}

export function SkeletonText({ lines = 2, className = '' }: { lines?: number; className?: string }) {
  return (
    <div className={`space-y-2 ${className}`}>
      {Array.from({ length: lines }).map((_, index) => (
        <Skeleton key={index} className={`h-3 ${index === lines - 1 ? 'w-2/3' : 'w-full'}`} />
      ))}
    </div>
  );
}

/** 会话列表行骨架：48px 圆头像 + 两行条 + 时间条 */
export function ConvItemSkeleton() {
  return (
    <div className="flex h-row-conv items-center gap-3 px-4">
      <Skeleton className="h-avatar-md w-avatar-md rounded-full" />
      <div className="min-w-0 flex-1 space-y-2">
        <Skeleton className="h-3.5 w-1/3" />
        <Skeleton className="h-3 w-3/4" />
      </div>
      <Skeleton className="h-3 w-8" />
    </div>
  );
}

export function ConvListSkeleton({ loading, rows = 6 }: { loading: boolean; rows?: number }) {
  const visible = useSkeletonVisible(loading);
  if (!visible) return null;
  return (
    <div aria-busy="true" aria-label="正在加载会话">
      {Array.from({ length: rows }).map((_, index) => (
        <ConvItemSkeleton key={index} />
      ))}
    </div>
  );
}

/** 消息骨架：对方/自己两种对齐 */
export function MessageSkeleton({ mine = false }: { mine?: boolean }) {
  return (
    <div className={`flex px-4 py-2 ${mine ? 'justify-end' : 'justify-start'}`}>
      {!mine && <Skeleton className="me-2 h-avatar-sm w-avatar-sm rounded-full" />}
      <div className={`flex flex-col space-y-2 ${mine ? 'items-end' : 'items-start'}`}>
        <Skeleton className={`h-3 w-20 ${mine ? 'self-end' : ''}`} />
        <Skeleton className={`h-10 rounded-lg ${mine ? 'w-48' : 'w-56'}`} />
      </div>
    </div>
  );
}

export function ChatSkeleton({ loading }: { loading: boolean }) {
  const visible = useSkeletonVisible(loading);
  if (!visible) return null;
  return (
    <div className="flex-1 overflow-hidden py-4" aria-busy="true">
      <MessageSkeleton />
      <MessageSkeleton mine />
      <MessageSkeleton />
    </div>
  );
}
