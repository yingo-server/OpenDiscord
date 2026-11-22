/**
 * Bubble（Fluent / WinUI 3）：消息气泡使用 Fluent 圆角（8px，尾角 2px）、
 * 低对比阴影（--shadow-card）与语义配色；发送中透明度、失败红点与状态角标保持不变。
 * 只负责气泡结构与状态角标，不含任何消息类型渲染。
 */
import { useRef, type MouseEvent as ReactMouseEvent, type PointerEvent as ReactPointerEvent, type ReactNode } from 'react';
import type { MessageStatus } from '../../core/types';
import { t } from '../i18n';
import { IconButton } from './IconButton';
import { CheckIcon, DoubleCheckIcon, SpinnerIcon } from './icons';

export interface BubbleProps {
  mine: boolean;
  status?: MessageStatus;
  timeLabel: string;
  children: ReactNode;
  deleted?: boolean;
  deletedLabel?: string;
  error?: string;
  senderName?: string;
  avatar?: ReactNode;
  showAvatar?: boolean;
  showSenderName?: boolean;
  /** 图片/视频气泡去内边距 */
  flush?: boolean;
  animate?: boolean;
  onRetry?: () => void;
  onLongPress?: () => void;
}

export function Bubble({
  mine,
  status,
  timeLabel,
  children,
  deleted = false,
  deletedLabel,
  error,
  senderName,
  avatar,
  showAvatar = false,
  showSenderName = false,
  flush = false,
  animate = false,
  onRetry,
  onLongPress,
}: BubbleProps) {
  const pressTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  if (deleted) {
    return (
      <div className="flex justify-center px-4 py-1.5">
        <span className="text-caption text-ink-tertiary">{deletedLabel ?? t('chat.deleted')}</span>
      </div>
    );
  }

  const clearPress = () => {
    if (pressTimer.current) {
      clearTimeout(pressTimer.current);
      pressTimer.current = null;
    }
  };

  const startPress = (event: ReactPointerEvent<HTMLDivElement>) => {
    if (event.pointerType === 'mouse') return; // 桌面走右键菜单
    clearPress();
    pressTimer.current = setTimeout(() => {
      pressTimer.current = null;
      onLongPress?.();
    }, 450);
  };

  const handleContextMenu = (event: ReactMouseEvent<HTMLDivElement>) => {
    if (!onLongPress) return;
    event.preventDefault();
    onLongPress();
  };

  return (
    <div
      className={[
        'group flex gap-2 px-4 py-1',
        mine ? 'flex-row-reverse' : 'flex-row',
        animate ? 'msg-enter' : '',
      ].join(' ')}
    >
      <div className="w-avatar-sm shrink-0 self-start">{showAvatar ? avatar : null}</div>

      <div
        className={`flex min-w-0 max-w-[78%] flex-col desktop:max-w-[60%] ${
          mine ? 'items-end' : 'items-start'
        }`}
      >
        {showSenderName && senderName ? (
          <span className="mb-1 text-caption text-ink-tertiary">{senderName}</span>
        ) : null}

        <div
          onContextMenu={handleContextMenu}
          onPointerDown={startPress}
          onPointerUp={clearPress}
          onPointerCancel={clearPress}
          onPointerLeave={clearPress}
          className={[
            'relative max-w-full rounded-lg text-body shadow-card transition-opacity duration-normal',
            flush ? 'overflow-hidden p-0' : 'px-3 py-2',
            mine
              ? 'rounded-br-tail bg-accent text-accent-fg'
              : 'rounded-bl-tail border border-line bg-surface text-ink',
            status === 'sending' ? 'opacity-60' : '',
          ].join(' ')}
        >
          {children}
        </div>

        <div className="mt-1 flex items-center gap-1.5 text-caption text-ink-tertiary">
          <span className="nums">{timeLabel}</span>
          {mine && status ? <StatusMark status={status} /> : null}
          {error ? (
            <IconButton
              label={t('chat.failed')}
              title={error || t('chat.failed')}
              size="sm"
              icon={<span className="block h-2 w-2 rounded-full bg-danger-500" />}
              onClick={onRetry}
              className="ms-0.5 h-3.5 w-3.5 rounded-full ring-2 ring-danger-500/20"
            />
          ) : null}
        </div>
      </div>
    </div>
  );
}

function StatusMark({ status }: { status: MessageStatus }) {
  if (status === 'sending') {
    return <SpinnerIcon size={12} className="text-ink-tertiary" />;
  }
  if (status === 'failed') {
    return <span className="h-2 w-2 rounded-full bg-danger-500" aria-label={t('chat.failed')} />;
  }
  if (status === 'read') {
    return <DoubleCheckIcon size={14} className="text-accent" />;
  }
  if (status === 'delivered') {
    return <DoubleCheckIcon size={14} />;
  }
  return <CheckIcon size={14} />;
}
