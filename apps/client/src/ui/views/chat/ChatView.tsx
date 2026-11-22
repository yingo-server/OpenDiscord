/**
 * 聊天窗：头部（标题/在线态）+ 网络条 + 消息列表 + 输入栏。
 * 不包含任何消息类型渲染逻辑。
 */
import { useEffect, useState } from 'react';
import { useConvStore } from '../../../core/stores/conv-store';
import { useMessageStore } from '../../../core/stores/message-store';
import { usePresenceStore } from '../../../core/stores/presence-store';
import { useConnStore } from '../../../core/stores/conn-store';
import { useSessionStore, isFlagEnabled } from '../../../core/stores/session-store';
import { selectIsMine, selectPresence, selectTypingUserIds } from '../../../core/selectors';
import type { ChatMessage, ConvRecord } from '../../../core/types';
import { t } from '../../i18n';
import { Avatar } from '../../components/Avatar';
import { NetworkBar } from '../../components/NetworkBar';
import { ChatSkeleton } from '../../components/Skeleton';
import { BackIcon, MuteIcon, MoreIcon, PhoneIcon, PinIcon, SpinnerIcon, VideoIcon } from '../../components/icons';
import { Button } from '../../components/Button';
import { IconButton } from '../../components/IconButton';
import { toast } from '../../components/Toast';
import { navigate } from '../../router';
import { MessageList } from './MessageList';
import { Composer } from './Composer';
import { useCall } from '../call/useCall';

export function ChatView({ convId }: { convId: number | null }) {
  if (convId === null) return <ChatPlaceholder />;
  return <ChatSession convId={convId} />;
}

function ChatSession({ convId }: { convId: number }) {
  const conv = useConvStore((state) => state.convs[convId]);
  const conn = useConnStore((state) => state.state);
  const ids = useMessageStore((state) => state.idsByConv[convId]);
  const selfId = useSessionStore((state) => state.userId);
  const otherId = conv && conv.convType === 1 ? (conv.members.find((member) => member.userId !== selfId)?.userId ?? 0) : 0;
  const presence = usePresenceStore((state) => (otherId ? selectPresence(state, otherId) : 'offline'));
  const callEnabled = useSessionStore((state) => isFlagEnabled(state.flags, 'call_enabled'));
  const call = useCall();
  const [replyTo, setReplyTo] = useState<ChatMessage | null>(null);
  const [scrollSignal, setScrollSignal] = useState(0);
  const [, tick] = useState(0);

  // 正在输入 3s TTL：每 2s 重算一次，驱动副标题更新
  useEffect(() => {
    const timer = setInterval(() => {
      usePresenceStore.getState().prune();
      tick((value) => value + 1);
    }, 2000);
    return () => clearInterval(timer);
  }, [convId]);

  useEffect(() => {
    setReplyTo(null);
  }, [convId]);

  const typingCount = usePresenceStore((state) => selectTypingUserIds(state, convId).length);

  if (!conv) {
    return (
      <section className="flex min-h-0 flex-1 flex-col bg-page">
        <div className="pt-safe flex h-14 items-center border-b border-line bg-surface px-3">
          <IconButton
            label={t('common.back')}
            onClick={() => {
              useConvStore.getState().setActive(null);
              navigate('/chats');
            }}
            icon={<BackIcon size={22} />}
          />
          <span className="fluent-title">{t('chat.selectConv')}</span>
        </div>
        <div className="flex flex-1 items-center justify-center">
          <SpinnerIcon size={24} className="text-ink-tertiary" />
        </div>
      </section>
    );
  }

  const title = conv.title || t('conv.untitled');
  const subtitle =
    typingCount > 0
      ? t('chat.typing')
      : conv.convType === 1 && presence === 'online'
        ? t('network.online')
        : conv.convType === 1
          ? ''
          : t('conv.members', { count: conv.members.length });

  return (
    <section className="flex min-h-0 flex-1 flex-col bg-page" aria-label={title}>
      <header className="pt-safe flex h-14 shrink-0 items-center gap-2 border-b border-line bg-surface px-2 tablet:px-4">
        <IconButton
          label={t('common.back')}
          onClick={() => {
            useConvStore.getState().setActive(null);
            navigate('/chats');
          }}
          icon={<BackIcon size={22} />}
        />

        <Avatar
          name={title}
          src={conv.avatar}
          size="sm"
          userId={otherId || conv.convId}
          presence={conv.convType === 1 ? presence : undefined}
        />

        <div className="min-w-0 flex-1">
          <h2 className="fluent-title truncate">{title}</h2>
          {subtitle ? (
            <p className={`truncate text-caption ${typingCount > 0 ? 'text-accent' : 'text-ink-tertiary'}`}>
              {subtitle}
            </p>
          ) : null}
        </div>

        {callEnabled ? (
          <CallButtons
            direct={conv.convType === 1}
            onStart={(media) => {
              if (conv.convType !== 1) {
                toast.info(t('call.groupUnsupported'));
                return;
              }
              call.startCall(convId, otherId, media);
            }}
          />
        ) : null}

        <MoreMenu
          convId={convId}
          pinned={Boolean(conv.pinned)}
          muted={Boolean(conv.muted)}
        />
      </header>

      <NetworkBar />

      <div className="flex min-h-0 flex-1 flex-col">
        {ids === undefined && conn !== 'online' ? (
          <ChatSkeleton loading />
        ) : (
          <MessageList
            convId={convId}
            scrollSignal={scrollSignal}
            onReply={(message) => setReplyTo(message)}
          />
        )}
      </div>

      <Composer
        convId={convId}
        replyTo={replyTo}
        replySenderName={replyTo ? senderNameOf(conv, replyTo.senderId, selfId) : undefined}
        onCancelReply={() => setReplyTo(null)}
        onSent={() => setScrollSignal((value) => value + 1)}
      />
    </section>
  );
}

function CallButtons({
  direct,
  onStart,
}: {
  direct: boolean;
  onStart: (media: 'audio' | 'video') => void;
}) {
  const tooltip = direct ? undefined : t('call.groupUnsupported');
  return (
    <>
      <IconButton
        label={direct ? t('call.startAudio') : t('call.groupUnsupported')}
        onClick={() => onStart('audio')}
        disabled={!direct}
        icon={<PhoneIcon size={20} />}
      />
      <IconButton
        label={direct ? t('call.startVideo') : t('call.groupUnsupported')}
        onClick={() => onStart('video')}
        disabled={!direct}
        icon={<VideoIcon size={20} />}
      />
    </>
  );
}

function senderNameOf(conv: ConvRecord, userId: number, selfId: number): string {
  if (userId === selfId) return t('common.me');
  return conv.members.find((member) => member.userId === userId)?.name ?? t('common.unknownUser', { id: userId });
}


/** 聊天窗「更多」下拉：置顶/静音快捷开关（conv-store 既有本地能力）。 */
function MoreMenu({ convId, pinned, muted }: { convId: number; pinned: boolean; muted: boolean }) {
  const [open, setOpen] = useState(false);
  const toggle = (action: () => void) => {
    action();
    setOpen(false);
  };
  return (
    <div className="relative">
      <IconButton
        label={t('common.more')}
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        icon={<MoreIcon size={20} />}
      />
      {open ? (
        <>
          <div className="fixed inset-0 z-menu" onClick={() => setOpen(false)} />
          <div
            role="menu"
            aria-label={t('common.more')}
            className="fluent-flyout absolute end-0 top-11 z-menu w-40 animate-scale-in py-1"
          >
            <Button
              variant="ghost"
              icon={<PinIcon size={18} />}
              onClick={() => toggle(() => useConvStore.getState().togglePin(convId))}
              className="h-10 w-full justify-start gap-2.5 text-start text-body-sm text-ink"
            >
              {pinned ? t('conv.unpin') : t('conv.pin')}
            </Button>
            <Button
              variant="ghost"
              icon={<MuteIcon size={18} />}
              onClick={() => toggle(() => useConvStore.getState().toggleMute(convId))}
              className="h-10 w-full justify-start gap-2.5 text-start text-body-sm text-ink"
            >
              {muted ? t('conv.unmute') : t('conv.mute')}
            </Button>
          </div>
        </>
      ) : null}
    </div>
  );
}

export function ChatPlaceholder() {
  return (
    <section className="hidden min-h-0 flex-1 flex-col items-center justify-center bg-page tablet:flex">
      <div className="mb-4 flex h-32 w-32 items-center justify-center rounded-full bg-surface-2 text-ink-tertiary">
        <MoreIcon size={48} />
      </div>
      <p className="fluent-title">{t('chat.selectConv')}</p>
      <p className="mt-1 text-body-sm text-ink-tertiary">{t('chat.selectConvDesc')}</p>
    </section>
  );
}
