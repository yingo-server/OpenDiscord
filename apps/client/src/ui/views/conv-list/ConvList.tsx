/**
 * 会话列表（§3.4）：行组件按 convId 订阅，未读数变化不触发兄弟项重渲染。
 * 搜索、置顶/静音快捷操作、空状态与骨架屏。
 */
import { memo, useMemo, useRef, useState } from 'react';
import { Tab, TabList } from '@fluentui/react-components';
import { useVirtualizer } from '@tanstack/react-virtual';
import { useConvStore } from '../../../core/stores/conv-store';
import { usePresenceStore } from '../../../core/stores/presence-store';
import { useConnStore } from '../../../core/stores/conn-store';
import type { ConvRecord } from '../../../core/types';
import { Avatar } from '../../components/Avatar';
import { NetworkBar } from '../../components/NetworkBar';
import { ConvListSkeleton } from '../../components/Skeleton';
import { MuteIcon, PinIcon, PlusIcon, SearchIcon, SettingsIcon } from '../../components/icons';
import { IconButton } from '../../components/IconButton';
import { Button } from '../../components/Button';
import { Input } from '../../components/Input';
import { t, type I18nKey } from '../../i18n';
import { formatConvTime } from '../../utils/format';
import { navigate } from '../../router';
import { toast } from '../../components/Toast';
import { getAppServices } from '../../services/app-services';
import { createEnvelope } from '../../../net/proto/codec';
import { encodeConvCreate } from '../../../net/proto/payload';
import { KINDS } from '../../../net/protocol/kinds';

const PREVIEW_KEYS: Record<string, I18nKey> = {
  image: 'conv.preview.image',
  audio: 'conv.preview.audio',
  video: 'conv.preview.video',
  file: 'conv.preview.file',
  system: 'conv.preview.system',
};

/** FE-1b：虚拟化开启条件 —— 连接中（骨架屏）或无可渲染会话（空状态）时不虚拟化。 */
export function shouldVirtualize(connecting: boolean, renderableCount: number): boolean {
  return !connecting && renderableCount > 0;
}

export function ConvList({ className = '' }: { className?: string }) {
  const order = useConvStore((state) => state.order);
  const convs = useConvStore((state) => state.convs);
  const conn = useConnStore((state) => state.state);
  const [query, setQuery] = useState('');
  const [createOpen, setCreateOpen] = useState(false);
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const connecting = conn === 'connecting' || conn === 'reconnecting';

  const filtered = useMemo(() => {
    const keyword = query.trim().toLowerCase();
    if (!keyword) return order;
    return order.filter((convId) => {
      const conv = convs[convId];
      if (!conv) return false;
      return (
        conv.title.toLowerCase().includes(keyword) ||
        (conv.lastPreview ?? '').toLowerCase().includes(keyword)
      );
    });
  }, [order, convs, query]);

  const virtualizer = useVirtualizer({
    count: shouldVirtualize(connecting, filtered.length) ? filtered.length : 0,
    getScrollElement: () => scrollRef.current,
    // 行高随内容/换行变化：动态测量（measureElement），估算值取移动端行高
    estimateSize: () => 72,
    overscan: 10,
    getItemKey: (index) => filtered[index] ?? index,
  });

  return (
    <section className={`flex min-h-0 flex-col bg-surface ${className}`} aria-label={t('conv.title')}>
      <header className="pt-safe flex h-14 shrink-0 items-center justify-between border-b border-line bg-surface px-4">
        <h1 className="fluent-title">{t('conv.title')}</h1>
        <div className="flex items-center gap-0.5">
          <IconButton
            label={t('conv.create.title')}
            onClick={() => setCreateOpen(true)}
            icon={<PlusIcon size={20} />}
          />
          <IconButton
            label={t('common.search')}
            onClick={() => toast.info(t('common.comingSoon'))}
            icon={<SearchIcon size={20} />}
            className="hidden desktop:inline-flex"
          />
          <IconButton
            label={t('settings.title')}
            onClick={() => navigate('/settings')}
            icon={<SettingsIcon size={20} />}
          />
        </div>
      </header>

      <NetworkBar />

      <div className="shrink-0 px-3 py-2">
        <div className="relative">
          <Input
            type="search"
            value={query}
            onChange={setQuery}
            placeholder={t('conv.search')}
            aria-label={t('conv.search')}
            icon={<SearchIcon size={16} />}
          />
        </div>
      </div>

      <div className="scroll-thin min-h-0 flex-1 overflow-y-auto pb-2" role="listbox" aria-label={t('conv.title')}>
        {order.length === 0 && (conn === 'connecting' || conn === 'reconnecting') ? (
          <ConvListSkeleton loading />
        ) : filtered.length === 0 ? (
          <EmptyConv search={query.trim().length > 0} onCreate={() => setCreateOpen(true)} />
        ) : (
          filtered.map((convId) => <ConvRow key={convId} convId={convId} />)
        )}
      </div>

      {createOpen ? <CreateConvDialog onClose={() => setCreateOpen(false)} /> : null}
    </section>
  );
}

const ConvRow = memo(function ConvRow({ convId }: { convId: number }) {
  const conv = useConvStore((state) => state.convs[convId]);
  const active = useConvStore((state) => state.activeConvId === convId);
  const draft = useConvStore((state) => state.drafts[convId]);
  const selfId = useConvStore((state) => state.selfUserId);
  const otherId =
    conv && conv.convType === 1 ? (conv.members.find((member) => member.userId !== selfId)?.userId ?? 0) : 0;
  const presence = usePresenceStore((state) => (otherId ? state.byUserId[otherId]?.status : undefined));

  if (!conv) return null;

  const preview = previewOf(conv, draft, selfId);
  const unread = conv.unread;
  const time = formatConvTime(conv.lastMessageAt ?? conv.updatedAt);
  const avatarName = conv.title || t('conv.untitled');

  return (
    <div
      role="option"
      aria-selected={active}
      tabIndex={0}
      onClick={() => {
        useConvStore.getState().setActive(convId);
        navigate(`/chat/${convId}`);
      }}
      onKeyDown={(event) => {
        if (event.key === 'Enter' || event.key === ' ') {
          event.preventDefault();
          useConvStore.getState().setActive(convId);
          navigate(`/chat/${convId}`);
        }
      }}
      aria-label={`${avatarName}${unread > 0 ? `，${t('conv.unread', { count: unread })}` : ''}`}
      className={[
        'group relative flex h-row-conv-mobile cursor-pointer items-center gap-3 border-b border-line/60 px-4 transition-colors duration-micro focus-visible:outline focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-focus',
        active
          ? 'border-s-[3px] border-s-accent bg-accent/[0.08] ps-[13px]'
          : 'hover:bg-ink/[0.04] active:bg-ink/[0.08]',
      ].join(' ')}
    >
      <Avatar name={avatarName} src={conv.avatar} size="md" presence={presence} userId={otherId || conv.convId} />

      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-1.5">
          <span className="min-w-0 truncate text-body font-medium text-ink">{avatarName}</span>
          {conv.pinned ? <PinIcon size={12} className="shrink-0 text-ink-tertiary" /> : null}
          {conv.muted ? <MuteIcon size={12} className="shrink-0 text-ink-tertiary" /> : null}
        </div>
        <p
          className={`mt-0.5 truncate text-body-sm ${draft ? 'text-danger-500' : 'text-ink-tertiary'}`}
        >
          {preview}
        </p>
      </div>

      <div className="flex h-full shrink-0 flex-col items-end justify-center gap-1 pt-1">
        <span className="nums text-caption text-ink-tertiary">{time}</span>
        {unread > 0 ? (
          <span
            className={[
              'nums inline-flex h-4 min-w-4 items-center justify-center rounded-full px-1 text-caption font-medium text-ink-inverse',
              conv.muted ? 'bg-ink-tertiary' : 'bg-danger-500',
            ].join(' ')}
          >
            {unread > 99 ? '99+' : unread}
          </span>
        ) : (
          <span className="h-4" aria-hidden="true" />
        )}
      </div>

      <div className="absolute end-2 top-1 hidden items-center gap-0.5 rounded-md border border-line bg-elevated p-0.5 shadow-md group-hover:flex">
        <IconButton
          label={conv.pinned ? t('conv.unpin') : t('conv.pin')}
          icon={<PinIcon size={16} />}
          onClick={(event) => {
            event.stopPropagation();
            useConvStore.getState().togglePin(convId);
          }}
        />
        <IconButton
          label={conv.muted ? t('conv.unmute') : t('conv.mute')}
          icon={<MuteIcon size={16} />}
          onClick={(event) => {
            event.stopPropagation();
            useConvStore.getState().toggleMute(convId);
          }}
        />
      </div>
    </div>
  );
});

function previewOf(conv: ConvRecord, draft: string | undefined, selfId: number): string {
  if (draft) return t('conv.draft', { text: draft });
  const type = conv.lastMessageType;
  if (type && type !== 'text' && type !== 'system' && PREVIEW_KEYS[type]) {
    return t(PREVIEW_KEYS[type]);
  }
  const text = conv.lastPreview ?? '';
  if (!text) return '';
  return conv.lastSenderId && conv.lastSenderId === selfId ? t('conv.you', { text }) : text;
}

function EmptyConv({ search, onCreate }: { search: boolean; onCreate: () => void }) {
  return (
    <div className="flex flex-col items-center justify-center px-6 py-16 text-center">
      <div className="mb-4 flex h-32 w-32 items-center justify-center rounded-full bg-surface-2 text-ink-tertiary">
        <SearchIcon size={48} />
      </div>
      <p className="fluent-title">{t(search ? 'conv.searchEmpty.title' : 'conv.empty.title')}</p>
      <p className="mt-1 text-body-sm text-ink-tertiary">
        {t(search ? 'conv.searchEmpty.desc' : 'conv.empty.desc')}
      </p>
      {!search ? (
        <Button variant="primary" onClick={onCreate} className="mt-4">
          {t('conv.empty.action')}
        </Button>
      ) : null}
    </div>
  );
}

function CreateConvDialog({ onClose }: { onClose: () => void }) {
  const [mode, setMode] = useState<1 | 2>(1);
  const [uid, setUid] = useState('');
  const [title, setTitle] = useState('');
  const [error, setError] = useState('');
  const [sending, setSending] = useState(false);

  const submit = () => {
    const target = Number(uid.trim());
    if (!Number.isSafeInteger(target) || target <= 0) {
      setError(t('conv.create.invalidUid'));
      return;
    }
    const groupTitle = title.trim();
    if (mode === 2 && (!groupTitle || [...groupTitle].length > 30)) {
      setError(t('conv.create.invalidTitle'));
      return;
    }
    const client = getAppServices().client;
    if (!client) {
      setError(t('conv.create.sendFailed'));
      return;
    }
    setSending(true);
    setError('');
    const sent = client.send(
      createEnvelope({
        kind: KINDS.CONV_CREATE,
        requestId: `conv-create-${Date.now()}`,
        payload: encodeConvCreate({
          title: mode === 2 ? groupTitle : '',
          convType: mode,
          memberIds: [target],
        }),
      }),
    );
    if (!sent) {
      setSending(false);
      setError(t('conv.create.sendFailed'));
      return;
    }
    onClose();
  };

  return (
    <div
      className="fixed inset-0 z-modal flex items-center justify-center bg-scrim/40 p-4 backdrop-blur-[2px]"
      onClick={onClose}
      role="presentation"
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-labelledby="create-conv-title"
        className="fluent-flyout w-full max-w-[400px] animate-scale-in p-6"
        onClick={(event) => event.stopPropagation()}
      >
        <h2 id="create-conv-title" className="fluent-title">
          {t('conv.create.title')}
        </h2>
        <p className="mt-1 text-body-sm text-ink-tertiary">{t('conv.create.hint')}</p>

        <TabList
          className="mt-4 rounded-md bg-surface-2 p-0.5"
          selectedValue={mode}
          onTabSelect={(_, data) => {
            setMode(data.value as 1 | 2);
            setError('');
          }}
        >
          <Tab value={1} className="flex-1">{t('conv.create.direct')}</Tab>
          <Tab value={2} className="flex-1">{t('conv.create.group')}</Tab>
        </TabList>

        <Input
          id="create-conv-uid"
          label={t('conv.create.uid')}
          type="text"
          inputMode="numeric"
          value={uid}
          onChange={(value) => setUid(value.replace(/[^0-9]/g, ''))}
          placeholder={t('conv.create.uidPlaceholder')}
          className="mt-1.5"
        />

        {mode === 2 ? (
          <Input
            id="create-conv-title-input"
            label={t('conv.create.groupTitle')}
            type="text"
            value={title}
            maxLength={30}
            onChange={setTitle}
            placeholder={t('conv.create.groupTitlePlaceholder')}
            className="mt-3"
          />
        ) : null}

        {error ? (
          <p role="alert" className="fluent-error mt-2 inline-flex">
            {error}
          </p>
        ) : null}

        <div className="mt-6 flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose}>
            {t('common.cancel')}
          </Button>
          <Button variant="primary" onClick={submit} loading={sending}>
            {t('conv.create.submit')}
          </Button>
        </div>
      </div>
    </div>
  );
}
