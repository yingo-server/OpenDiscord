/**
 * MessageList —— 只负责位置与交互：虚拟滚动 / 长按菜单 / 滚动锚定。
 * 任何一种消息的渲染都在 renderers 注册表里，本文件不含类型分支。
 */
import {
  memo,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { createPortal } from 'react-dom';
import { useVirtualizer } from '@tanstack/react-virtual';
import { computeLoadThreshold, computeScrollCompensation } from './scroll-math';
import { historyLoader } from '../../../net/sync/history-loader';
import { useMessageStore } from '../../../core/stores/message-store';
import { useConvStore } from '../../../core/stores/conv-store';
import { useSessionStore } from '../../../core/stores/session-store';
import { selectIsMine } from '../../../core/selectors';
import type { ChatMessage, ConvRecord } from '../../../core/types';
import { getRenderer, getRendererChrome } from '../../renderers/registry';
import { Bubble } from '../../components/Bubble';
import { Avatar } from '../../components/Avatar';
import { SpinnerIcon, AlertIcon, CopyIcon, FlagIcon, ForwardIcon, ReplyIcon, SelectIcon, TrashIcon } from '../../components/icons';
import { t } from '../../i18n';
import { formatDayDivider, formatTime } from '../../utils/format';
import { discardSend, retrySend, recallMessage, forwardMessages, reportMessage } from '../../services/app-services';
import { toast } from '../../components/Toast';

const EMPTY_IDS: string[] = [];
const FIVE_MINUTES = 5 * 60 * 1000;

export interface MessageListProps {
  convId: number;
  onReply: (message: ChatMessage) => void;
  /** 发送成功后强制贴底（用户上滚时也回到底部） */
  scrollSignal?: number;
}

interface MenuState {
  messageId: string;
  x: number;
  y: number;
}

export function MessageList({ convId, onReply, scrollSignal = 0 }: MessageListProps) {
  const ids = useMessageStore((state) => state.idsByConv[convId] ?? EMPTY_IDS);
  const loadingHistory = useMessageStore((state) => state.loadingHistory[convId] ?? false);
  const unreadTotal = useConvStore((state) => state.convs[convId]?.unread ?? 0);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const [menu, setMenu] = useState<MenuState | null>(null);
  const [newCount, setNewCount] = useState(0);
  const [atBottom, setAtBottom] = useState(true);
  const atBottomRef = useRef(true);
  const [unreadIndex, setUnreadIndex] = useState<number | null>(null);
  // 多选态：组件本地 state（不进全局 store），selectedIdSet 与会话无关
  const [selectMode, setSelectMode] = useState(false);
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [forwardTarget, setForwardTarget] = useState<ChatMessage[] | null>(null);

  const enterSelectMode = useCallback((message: ChatMessage) => {
    setSelectMode(true);
    setSelectedIds(new Set([message.id]));
  }, []);

  const toggleSelected = useCallback((id: string) => {
    setSelectedIds((previous) => {
      const next = new Set(previous);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const exitSelectMode = useCallback(() => {
    setSelectMode(false);
    setSelectedIds(new Set());
  }, []);

  const openForwardPanel = useCallback(
    (messages: ChatMessage[]) => {
      if (messages.length === 0) {
        toast.info(t('chat.select.empty'));
        return;
      }
      setForwardTarget(messages);
    },
    [],
  );

  const virtualizer = useVirtualizer({
    count: ids.length,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => 72,
    overscan: 8,
    getItemKey: (index) => ids[index] ?? index,
  });

  const prevCount = useRef(0);
  const prevFirst = useRef<string | undefined>(undefined);
  const prevLast = useRef<string | undefined>(undefined);
  const initialized = useRef(false);
  const anchorId = useRef<string | null>(null);
  // 锚定补偿：记录插入前 scrollHeight/scrollTop（perf-spec §1.3）
  const anchorHeight = useRef(0);
  const anchorTop = useRef(0);
  // 滚动速度采样：px（上次 scrollTop）/ t（上次时间戳）/ last（再上次，用于判断方向）
  const scrollVel = useRef({ px: 0, t: 0, last: 0 });

  useEffect(() => {
    atBottomRef.current = atBottom;
  }, [atBottom]);

  // 切换会话：重置全部滚动锚点状态
  useEffect(() => {
    prevCount.current = 0;
    prevFirst.current = undefined;
    prevLast.current = undefined;
    initialized.current = false;
    anchorId.current = null;
    setNewCount(0);
    setAtBottom(true);
    atBottomRef.current = true;
    setUnreadIndex(null);
  }, [convId]);

  // 首次渲染：有未读 → 停到未读分隔线；否则贴底
  useEffect(() => {
    if (initialized.current || ids.length === 0) return;
    initialized.current = true;
    const target = unreadTotal > 0 ? Math.max(0, ids.length - unreadTotal) : ids.length - 1;
    if (unreadTotal > 0) setUnreadIndex(target);
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        virtualizer.scrollToIndex(target, { align: unreadTotal > 0 ? 'start' : 'end' });
      });
    });
    prevCount.current = ids.length;
    prevFirst.current = ids[0];
    prevLast.current = ids[ids.length - 1];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids.length, convId, unreadTotal]);

  // 新消息 / 历史加载：保持锚点，不把用户视口顶走
  useEffect(() => {
    const count = ids.length;
    const previous = prevCount.current;
    if (initialized.current && count !== previous) {
      const appended = previous > 0 && ids[ids.length - 1] !== prevLast.current && ids[0] === prevFirst.current;
      const prepended = previous > 0 && ids[0] !== prevFirst.current && ids[ids.length - 1] === prevLast.current;
      if (appended) {
        if (atBottomRef.current) {
          requestAnimationFrame(() => virtualizer.scrollToIndex(count - 1, { align: 'end' }));
        } else {
          setNewCount((value) => value + (count - previous));
        }
      } else if (prepended && anchorId.current) {
        const target = ids.indexOf(anchorId.current);
        anchorId.current = null;
        if (target >= 0) {
          requestAnimationFrame(() => virtualizer.scrollToIndex(target, { align: 'start' }));
        }
      }
    }
    prevCount.current = count;
    prevFirst.current = ids[0];
    prevLast.current = ids[ids.length - 1];
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ids]);

  // 停留 1.2s 后回写已读（分隔线保持到离开会话）
  useEffect(() => {
    if (unreadTotal <= 0) return;
    const timer = setTimeout(() => useConvStore.getState().markRead(convId), 1200);
    return () => clearTimeout(timer);
  }, [convId, unreadTotal]);

  const loadOlder = useCallback(() => {
    if (loadingHistory) return;
    if (historyLoader.isExhausted(convId)) return; // 已到底：不再请求
    const element = scrollRef.current;
    const state = useMessageStore.getState();
    const first = state.byId[ids[0]] as ChatMessage | undefined;
    const firstSeq = first?.seq ?? 0;
    if (!firstSeq || firstSeq <= 1) return;
    // 锚定：插入更早消息后 scrollHeight 变大；恢复"距底距离"使视口内容不位移
    if (anchorHeight.current > 0) {
      requestAnimationFrame(() => {
        const el = scrollRef.current;
        if (!el) return;
        el.scrollTop = computeScrollCompensation(
          anchorHeight.current,
          anchorTop.current,
          el.scrollHeight,
        );
      });
      anchorHeight.current = 0;
    }
    const firstVisible = virtualizer.getVirtualItems()[0];
    anchorId.current = firstVisible ? ids[firstVisible.index] ?? null : ids[0] ?? null;
    anchorHeight.current = element?.scrollHeight ?? 0;
    anchorTop.current = element?.scrollTop ?? 0;
    void state.loadOlder(convId, firstSeq);
  }, [convId, ids, loadingHistory, virtualizer]);

  const handleScroll = useCallback(() => {
    const element = scrollRef.current;
    if (!element) return;
    const distance = element.scrollHeight - element.scrollTop - element.clientHeight;
    const bottom = distance < 120;
    setAtBottom(bottom);
    if (bottom) setNewCount(0);
    // 动态阈值：慢速 200px，快速上滑放大到 800px（perf-spec §1.3 预取）
    const now = performance.now();
    const velocity = scrollVel.current.px / Math.max(1, now - scrollVel.current.t);
    scrollVel.current = { px: element.scrollTop, t: now, last: scrollVel.current.last };
    const rising = element.scrollTop < scrollVel.current.last; // 上滑
    const threshold = rising ? computeLoadThreshold(Math.abs(velocity)) : 200;
    if (element.scrollTop < threshold) loadOlder();
  }, [loadOlder]);

  const openMenu = useCallback((messageId: string) => {
    const element = scrollRef.current;
    const rect = element?.getBoundingClientRect();
    setMenu({
      messageId,
      x: Math.max(8, Math.min((rect?.left ?? 0) + 24, window.innerWidth - 220)),
      y: Math.max(8, Math.min((rect?.top ?? 0) + 80, window.innerHeight - 300)),
    });
  }, []);

  useEffect(() => {
    if (!scrollSignal || ids.length === 0) return;
    setNewCount(0);
    setAtBottom(true);
    requestAnimationFrame(() => virtualizer.scrollToIndex(ids.length - 1, { align: 'end' }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scrollSignal]);

  if (ids.length === 0) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center px-6 text-center">
        <div className="mb-4 flex h-32 w-32 items-center justify-center rounded-full bg-surface-2 text-ink-tertiary">
          <ReplyIcon size={48} />
        </div>
        <p className="text-title text-ink">{t('chat.empty.title')}</p>
        <p className="mt-1 text-body-sm text-ink-tertiary">{t('chat.empty.desc')}</p>
      </div>
    );
  }

  return (
    <div className="relative flex min-h-0 flex-1 flex-col">
      <div
        ref={scrollRef}
        onScroll={handleScroll}
        className="scroll-thin min-h-0 flex-1 overflow-y-auto px-0 py-4"
        role="log"
        aria-live="polite"
        aria-label={t('conv.title')}
      >
        <div style={{ height: virtualizer.getTotalSize(), position: 'relative', width: '100%' }}>
          {virtualizer.getVirtualItems().map((item) => {
            const id = ids[item.index];
            return (
              <div
                key={id}
                data-index={item.index}
                ref={virtualizer.measureElement}
                style={{
                  position: 'absolute',
                  insetInline: 0,
                  top: 0,
                  transform: `translateY(${item.start}px)`,
                }}
              >
                <MessageRow
                  id={id}
                  prevId={item.index > 0 ? ids[item.index - 1] : undefined}
                  isUnreadStart={unreadIndex === item.index}
                  onOpenMenu={openMenu}
                  onReply={onReply}
                  selectMode={selectMode}
                  selected={selectedIds.has(id)}
                  onToggleSelect={toggleSelected}
                />
              </div>
            );
          })}
        </div>
      </div>

      {loadingHistory ? (
        <div className="pointer-events-none absolute inset-x-0 top-0 flex justify-center py-2">
          <span className="fluent-flyout inline-flex items-center gap-1.5 rounded-full px-3 py-1 text-caption text-ink-tertiary">
            <SpinnerIcon size={12} />
            {t('chat.loadingOlder')}
          </span>
        </div>
      ) : null}

      {!atBottom && newCount > 0 ? (
        <button
          type="button"
          onClick={() => {
            virtualizer.scrollToIndex(ids.length - 1, { align: 'end' });
            setNewCount(0);
          }}
          className="fluent-focus absolute bottom-3 end-4 z-sticky inline-flex items-center gap-1.5 rounded-full bg-accent px-3 py-1.5 text-caption text-accent-fg shadow-md animate-slide-up hover:bg-accent-hover"
        >
          ↓ {t('chat.newMessages', { count: newCount })}
        </button>
      ) : null}

      {menu ? (
        <MessageMenu
          state={menu}
          onClose={() => setMenu(null)}
          onReply={(message) => {
            onReply(message);
            setMenu(null);
          }}
          onForward={(message) => {
            setMenu(null);
            openForwardPanel([message]);
          }}
          onSelect={(message) => {
            setMenu(null);
            enterSelectMode(message);
          }}
        />
      ) : null}

      {selectMode ? (
        <SelectBar
          count={selectedIds.size}
          onForward={() => {
            const messages = [...selectedIds]
              .map((id) => useMessageStore.getState().byId[id])
              .filter((message): message is ChatMessage => Boolean(message));
            openForwardPanel(messages);
          }}
          onCancel={exitSelectMode}
        />
      ) : null}

      {forwardTarget ? (
        <ForwardPanel
          messages={forwardTarget}
          onClose={() => setForwardTarget(null)}
        />
      ) : null}
    </div>
  );
}

// ---------------------------------------------------------------------------
// 多选底栏（最小可用）：已选数 + 转发 / 取消
// ---------------------------------------------------------------------------
function SelectBar({
  count,
  onForward,
  onCancel,
}: {
  count: number;
  onForward: () => void;
  onCancel: () => void;
}) {
  return (
    <div
      className="flex h-14 shrink-0 items-center justify-between border-t border-line bg-surface px-4"
      role="toolbar"
      aria-label={t('chat.selectMode.title', { count })}
    >
      <button
        type="button"
        onClick={onCancel}
        className="fluent-focus h-9 rounded-sm px-4 text-body-sm text-ink-secondary hover:bg-ink/[0.06]"
      >
        {t('chat.select.cancel')}
      </button>
      <span className="text-body-sm text-ink-tertiary">
        {t('chat.selectMode.title', { count })}
      </span>
      <button
        type="button"
        onClick={onForward}
        disabled={count === 0}
        className="fluent-focus h-9 rounded-sm bg-accent px-4 text-body-sm text-accent-fg hover:bg-accent-hover disabled:opacity-40"
      >
        {t('chat.select.forward')}
      </button>
    </div>
  );
}

// ---------------------------------------------------------------------------
// 转发面板：会话选择列表（useConvStore.convs），选定后走 outbox 发送
// ---------------------------------------------------------------------------
function ForwardPanel({
  messages,
  onClose,
}: {
  messages: ChatMessage[];
  onClose: () => void;
}) {
  const order = useConvStore((state) => state.order);
  const convs = useConvStore((state) => state.convs);
  const [sending, setSending] = useState(false);

  const forward = async (convId: number) => {
    if (sending) return;
    setSending(true);
    let ok = 0;
    for (const message of messages) {
      if (await forwardMessages(message, convId)) ok += 1;
    }
    setSending(false);
    onClose();
    if (ok === messages.length) toast.success(t('chat.forward.done', { count: ok }));
    else toast.danger(t('chat.forward.failed'));
  };

  return createPortal(
    <div className="fixed inset-0 z-menu flex items-end justify-center sm:items-center" onClick={onClose}>
      <div
        role="dialog"
        aria-label={t('chat.forward.title')}
        className="fluent-flyout max-h-[60vh] w-80 overflow-y-auto py-1 sm:rounded-md"
        onClick={(event) => event.stopPropagation()}
      >
        <p className="px-3 py-2 text-caption text-ink-tertiary">{t('chat.forward.title')}</p>
        {order.map((convId) => {
          const conv = convs[convId];
          if (!conv) return null;
          return (
            <button
              key={convId}
              type="button"
              onClick={() => void forward(convId)}
              disabled={sending}
              className="flex h-12 w-full items-center gap-3 px-3 text-start transition-colors duration-micro hover:bg-surface-2 disabled:opacity-50"
            >
              <Avatar name={conv.title} size="sm" />
              <span className="min-w-0 flex-1 truncate text-body-sm text-ink">{conv.title}</span>
            </button>
          );
        })}
      </div>
    </div>,
    document.body,
  );
}

interface RowProps {
  id: string;
  prevId?: string;
  isUnreadStart: boolean;
  onOpenMenu: (messageId: string) => void;
  onReply: (message: ChatMessage) => void;
  selectMode?: boolean;
  selected?: boolean;
  onToggleSelect?: (id: string) => void;
}

const MessageRow = memo(function MessageRow({
  id,
  prevId,
  isUnreadStart,
  onOpenMenu,
  onReply,
  selectMode = false,
  selected = false,
  onToggleSelect,
}: RowProps) {
  const message = useMessageStore((state) => state.byId[id]);
  const prev = useMessageStore((state) => (prevId ? state.byId[prevId] : undefined));
  const conv = useConvStore((state) => state.convs[message?.convId ?? 0]);
  const userId = useSessionStore((state) => state.userId);

  if (!message) return null;

  const mine = selectIsMine(message, userId);
  const showDate = !prev || dayKey(prev.sentAt) !== dayKey(message.sentAt);
  const gap = prev ? message.sentAt - prev.sentAt : Number.POSITIVE_INFINITY;
  const grouped = Boolean(prev) && gap < FIVE_MINUTES && prev?.senderId === message.senderId;
  const showAvatar = !mine && !grouped;
  const showSenderName = !mine && !grouped && Boolean(conv && conv.convType !== 1);
  const senderName = memberName(conv, message.senderId);
  const Renderer = getRenderer(message.type) ?? getRenderer('unknown');
  const plain = getRendererChrome(message.type) === 'plain';

  return (
    <div>
      {showDate ? <DayDivider timestamp={message.sentAt} /> : null}
      {isUnreadStart ? <UnreadDivider /> : null}

      <div className="flex items-center">
        {selectMode ? (
          <button
            type="button"
            role="checkbox"
            aria-checked={selected}
            aria-label={t('chat.select')}
            onClick={() => onToggleSelect?.(id)}
            className={[
              'ms-2 inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-full border transition-colors duration-micro',
              selected ? 'border-accent bg-accent text-accent-fg' : 'border-line-input bg-surface text-ink-tertiary',
            ].join(' ')}
          >
            {selected ? '✓' : ''}
          </button>
        ) : null}

        {!Renderer ? null : plain ? (
        <Renderer message={message} />
      ) : (
        <Bubble
          mine={mine}
          status={message.status}
          timeLabel={formatTime(message.sentAt)}
          deleted={Boolean(message.deleted)}
          deletedLabel={mine ? t('chat.recalledByMe') : t('chat.recalled')}
          error={message.error}
          senderName={senderName}
          showSenderName={showSenderName}
          showAvatar={showAvatar}
          avatar={<Avatar name={senderName} size="sm" userId={message.senderId} />}
          flush={message.type === 'image'}
          animate={message.status === 'sending'}
          onRetry={() => {
            const localId = localIdOf(message);
            if (localId) retrySend(localId);
          }}
          onLongPress={() => onOpenMenu(message.id)}
        >
          <Renderer message={message} />
        </Bubble>
      )}
      </div>
    </div>
  );
});

function dayKey(timestamp: number): string {
  const date = new Date(timestamp);
  return `${date.getFullYear()}-${date.getMonth()}-${date.getDate()}`;
}

function DayDivider({ timestamp }: { timestamp: number }) {
  return (
    <div className="flex justify-center py-2">
      <span className="rounded-full bg-surface-2 px-2.5 py-0.5 text-caption text-ink-tertiary">
        {formatDayDivider(timestamp)}
      </span>
    </div>
  );
}

function UnreadDivider() {
  return (
    <div className="flex items-center gap-3 px-6 py-2">
      <span className="h-px flex-1 bg-accent/40" />
      <span className="text-caption text-accent">{t('chat.unreadBelow')}</span>
      <span className="h-px flex-1 bg-accent/40" />
    </div>
  );
}

function memberName(conv: ConvRecord | undefined, userId: number): string {
  if (!conv) return t('common.unknownUser', { id: userId });
  const member = conv.members.find((candidate) => candidate.userId === userId);
  return member?.name || conv.title || t('common.unknownUser', { id: userId });
}

function localIdOf(message: ChatMessage): string | null {
  if (!message.id.startsWith('local:')) return null;
  return `l_${message.id.slice('local:'.length)}`;
}

// ---------------------------------------------------------------------------
// 长按 / 右键菜单
// ---------------------------------------------------------------------------

interface MenuItem {
  key: string;
  label: string;
  icon: ReactNode;
  danger?: boolean;
  action: () => void;
}

function MessageMenu({
  state,
  onClose,
  onReply,
  onForward,
  onSelect,
}: {
  state: MenuState;
  onClose: () => void;
  onReply: (message: ChatMessage) => void;
  onForward: (message: ChatMessage) => void;
  onSelect: (message: ChatMessage) => void;
}) {
  const message = useMessageStore((store) => store.byId[state.messageId]);
  const userId = useSessionStore((store) => store.userId);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [onClose]);

  if (!message) return null;
  const mine = selectIsMine(message, userId);
  const localId = localIdOf(message);
  const text = typeof message.payload.text === 'string' ? message.payload.text : '';

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      toast.success(t('toast.copied'));
    } catch {
      toast.danger(t('toast.serverError', { reason: 'clipboard' }));
    }
    onClose();
  };

  const items: MenuItem[] = [];
  if (localId) {
    items.push({
      key: 'retry',
      label: t('menu.retrySend'),
      icon: <AlertIcon size={18} />,
      action: () => {
        retrySend(localId);
        onClose();
      },
    });
    items.push({
      key: 'discard',
      label: t('menu.deleteLocal'),
      icon: <TrashIcon size={18} />,
      danger: true,
      action: () => {
        discardSend(localId);
        onClose();
      },
    });
  } else {
    items.push({
      key: 'reply',
      label: t('chat.reply'),
      icon: <ReplyIcon size={18} />,
      action: () => onReply(message),
    });
    if (text) {
      items.push({
        key: 'copy',
        label: t('chat.copy'),
        icon: <CopyIcon size={18} />,
        action: () => void copy(),
      });
    }
    items.push(
      {
        key: 'forward',
        label: t('chat.forward'),
        icon: <ForwardIcon size={18} />,
        action: () => onForward(message),
      },
      {
        key: 'select',
        label: t('chat.select'),
        icon: <SelectIcon size={18} />,
        action: () => onSelect(message),
      },
      {
        key: 'report',
        label: t('chat.report'),
        icon: <FlagIcon size={18} />,
        // v1.5：举报上行链路已实现（服务端 453 REPORT 落 sys_reports），
        // 直接发帧；服务端 ACK 即受理，失败 fail-closed 仅提示，不重试风暴。
        action: () => {
          const ok = reportMessage(message);
          toast.info(ok ? t('chat.reportSent') : t('chat.reportFailed'));
          onClose();
        },
      },
    );
    if (mine && Date.now() - message.sentAt < 2 * 60_000) {
      items.push({
        key: 'recall',
        label: t('chat.recall'),
        icon: <TrashIcon size={18} />,
        danger: true,
        action: () => {
          if (!recallMessage(message)) toast.danger(t('chat.recallFailed'));
          onClose();
        },
      });
    }
  }

  return createPortal(
    <div
      className="fixed inset-0 z-menu"
      onClick={onClose}
      onContextMenu={(event) => {
        event.preventDefault();
        onClose();
      }}
    >
      <div
        role="menu"
        aria-label={t('menu.message')}
        className="fluent-flyout absolute w-52 animate-scale-in py-1"
        style={{ left: state.x, top: state.y }}
        onClick={(event) => event.stopPropagation()}
      >
        {items.map((item) => (
          <button
            key={item.key}
            type="button"
            role="menuitem"
            onClick={item.action}
            className={[
              'flex h-10 w-full items-center gap-2.5 px-3 text-start text-body-sm transition-colors duration-micro hover:bg-ink/[0.06]',
              item.danger ? 'text-danger-500' : 'text-ink',
            ].join(' ')}
          >
            <span className="text-current opacity-80">{item.icon}</span>
            {item.label}
          </button>
        ))}
      </div>
    </div>,
    document.body,
  );
}
