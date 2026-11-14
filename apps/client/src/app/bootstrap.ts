/**
 * 应用接线（composition root）——main.tsx 只负责注册与挂载，具体装配集中在这里。
 *
 * 依赖方向：本文件可 import net/core/ui；core/net 不可反向 import 本文件。
 */
import { openCache } from '../net/cache/idb';
import { SyncEngine, type SyncSink } from '../net/sync/engine';
import { historyLoader } from '../net/sync/history-loader';
import { configurePullScheduler, pullScheduler, type PullSchedulerDeps } from '../net/sync/pull-scheduler';
import { configureMailboxSync, mailboxSync } from '../net/sync/mailbox';
import { Outbox, createLocalStoragePersistence } from '../net/sync/outbox';
import { createEnvelope } from '../net/proto/codec';
import { encodeSyncRequest } from '../net/proto/payload';
import { KINDS } from '../net/protocol/kinds';
import { dispatch } from '../net/protocol/dispatch';
import type { HandlerContext, HandlerRegistry, SessionPort, ConvPort, PresencePort, MessagePort } from '../net/protocol/registry';
import { WsClient } from '../net/transport/ws-client';
import type { NetClient } from '../net/transport/types';
import { TokenStore } from '../net/auth/token-store';
import { createHttpAuthApi, type AuthSession } from '../net/auth/provider';
import { MockBridge, createMockAuthApi, setActiveBridge } from '../net/mock/mockBridge';
import { useConnStore } from '../core/stores/conn-store';
import { useSessionStore } from '../core/stores/session-store';
import { useConvStore } from '../core/stores/conv-store';
import { useMessageStore } from '../core/stores/message-store';
import { usePresenceStore } from '../core/stores/presence-store';
import { navigate } from '../ui/router';
import { getThemePref, applyTheme, watchSystemTheme } from '../ui/theme';
import { toast } from '../ui/components/Toast';
import { t } from '../ui/i18n';
import { setAppServices } from '../ui/services/app-services';
import { callController } from '../net/call/controller';
import { ensureNotificationPermission, registerServiceWorker, subscribeAndReport } from '../net/push';

const IS_MOCK = import.meta.env.VITE_MOCK === '1';

/** PWA / Web Push（FE-2）：production only，dev/mock 不注册；全程 fail-closed。 */
async function initPush(): Promise<void> {
  const registration = await registerServiceWorker();
  if (!registration) return;
  if (!(await ensureNotificationPermission())) {
    console.info('[push] notification permission not granted, skip subscribe');
    return;
  }
  // 服务端接口缝未实现时内部 fail-closed，只记日志。
  void subscribeAndReport({ registration });
  // SW 点击通知 → focus 会话。
  navigator.serviceWorker.addEventListener('message', (event) => {
    const data = event.data as { type?: string; convId?: unknown } | null;
    if (data?.type === 'push-focus-conv' && typeof data.convId === 'number' && data.convId > 0) {
      navigate(`/chat/${data.convId}`);
    }
  });
}

export async function bootstrap(registry: HandlerRegistry): Promise<void> {
  applyTheme(getThemePref());
  watchSystemTheme();
  void initPush();

  const tokenStore = new TokenStore();
  const cache = await openCache();

  // 引用占位：sink.onGap 需要 client，而 client 需要 engine；两者在启动后才互相使用。
  let client: NetClient | null = null;

  const sink: SyncSink = {
    hydrate(data) {
      useConvStore.getState().hydrate(data.convs);
      useMessageStore.getState().hydrate(data.messages);
    },
    applyMessages(messages) {
      useMessageStore.getState().upsertMessages(messages);
    },
    applyTombstone(convId, msgId, seq, op) {
      useMessageStore.getState().applyTombstone(convId, msgId, seq, op);
    },
    applyEdit(convId, msgId, text) {
      useMessageStore.getState().applyEdit(convId, msgId, text);
    },
    setSyncState(convId, state) {
      useConvStore.getState().setSyncState(convId, state);
    },
    onGap(convId, gap) {
      // 缺帧 → 主动 SYNC_REQUEST（与 RESUME 共用同一 ingest 入口）
      if (!client) return;
      client.send(
        createEnvelope({
          kind: KINDS.SYNC_REQUEST,
          convId,
          payload: encodeSyncRequest({
            convId,
            sinceSeq: gap.from,
            limit: Math.min(200, Math.max(1, gap.to - gap.from + 1)),
          }),
        }),
      );
    },
    onError(error, context) {
      console.error(`[sync] ${context}`, error);
    },
  };

  const engine = new SyncEngine(cache, sink);
  await engine.hydrate();

  // 拉取模式调度器（spec v1.2 §2）：只产生 8 SYNC 请求，结果经既有 102 帧路径进 ingest。
  const pullSchedulerDeps: PullSchedulerDeps = {
    getLastSeq: (convId) => engine.lastSeq(convId),
    isConvLoaded: (convId) => useConvStore.getState().convs[convId] !== undefined,
    sendSync: (convId, sinceSeq, limit) =>
      net.send(
        createEnvelope({
          kind: KINDS.SYNC_REQUEST,
          convId,
          payload: encodeSyncRequest({ convId, sinceSeq, limit }),
        }),
      ),
  };
  configurePullScheduler(pullSchedulerDeps);
  // 会话 hydrate：conv-store 有会话落位后消费 pendingWatermark（spec §2.1 第 1 步的兑现）
  useConvStore.subscribe((state, previous) => {
    if (state.convs === previous.convs) return;
    for (const convId of Object.keys(state.convs)) {
      const id = Number(convId);
      if (previous.convs[id] === undefined) pullScheduler.consumePending(id);
    }
  });

  const outbox = new Outbox(
    {
      onOptimistic: (item) => useMessageStore.getState().applyOptimistic(item),
      onSending: () => undefined,
      onSent: (item, ack) => useMessageStore.getState().confirmLocal(item, ack),
      onFailed: (item, reason) => useMessageStore.getState().failLocal(item, reason),
      onRemoved: (localId) => useMessageStore.getState().removeLocal(localId),
      onQueueChange: () => undefined,
    },
    createLocalStoragePersistence(),
  );
  await outbox.load();

  // 会话元数据/未读/摘要随 store 变化落库（IndexedDB 为事实源，刷新后立即可渲染）
  useConvStore.subscribe((state, previous) => {
    if (state.convs === previous.convs) return;
    for (const [key, conv] of Object.entries(state.convs)) {
      if (previous.convs[Number(key)] !== conv) void cache.putConv(conv);
    }
  });

  // 会话恢复：UI 先离线渲染 IndexedDB，连接建立后增量收敛
  const stored = tokenStore.current;
  if (stored) {
    useSessionStore.getState().signIn(stored);
    useConvStore.getState().setSelfUserId(stored.userId);
    usePresenceStore.getState().setSelfUserId(stored.userId);
  }

  const mock = IS_MOCK ? new MockBridge() : null;
  if (mock) {
    client = mock;
    setActiveBridge(mock);
  } else {
    client = new WsClient({
      url: resolveWsUrl(),
      token: () => tokenStore.accessToken,
      deviceId: () => tokenStore.getDeviceId(),
      sessionId: () => tokenStore.sessionId,
      resumeTickets: () => engine.resumeTickets(),
      onFrame: () => undefined,
      format: 'json',
    });
  }
  const net = client;
  net.setResumeProvider(() => engine.resumeTickets());
  outbox.attach(net);

  // v3 mailbox 游标状态机（docs/v3-bus-design.md §2/§4）：ACK(15)/RESUME(17) 上行依赖注入。
  // ingest 收敛路径（handleMsgNew → engine.ingest 成功）负责调用 mailboxSync.ingest；
  // 本模块只做游标与发送决策，不写缓存。重连（online）→ 发 kind 17 续传。
  configureMailboxSync({
    send: (kind, payload) => net.send(createEnvelope({ kind, payload })),
    engineLastSeq: (convId) => engine.lastSeq(convId),
  });
  useConnStore.subscribe((state, previous) => {
    if (state.state === previous.state) return;
    if (state.state === 'online') mailboxSync.onConnState('online');
    else if (state.state === 'offline') mailboxSync.onConnState('offline');
  });

  // 上滑历史：三段式（本地 → 网络 920/921 → 超时降级），docs/perf-spec-v1.2.md §1.2。
  historyLoader.configure({
    cache: {
      listMessages: (convId, limit, beforeSeq) => cache.listMessages(convId, limit, beforeSeq),
      putMessages: (messages) => cache.putMessages(messages),
    },
    send: {
      requestHistory: (convId, beforeSeq, limit) => net.requestHistory(convId, beforeSeq, limit),
    },
    timeoutMs: 3000,
  });
  useMessageStore.getState().setHistoryLoader((convId, limit, beforeSeq) =>
    historyLoader
      .loadOlder(convId, beforeSeq ?? 0, limit)
      .then((o) => o.messages),
  );

  const sessionPort: SessionPort = {
    get userId() {
      return useSessionStore.getState().userId;
    },
    setFlags: (flags, version) => useSessionStore.getState().setFlags(flags, version),
    kicked: (reason, bannedUntil) => {
      tokenStore.clear();
      useSessionStore.getState().kicked(reason, bannedUntil);
      net.stop();
      useMessageStore.getState().clear();
      useConvStore.getState().clear();
      engine.reset();
      void outbox.clear();
    },
    rateLimited: (scope, retryAfterMs) => {
      toast.show(t('toast.rateLimited', { seconds: Math.max(1, Math.ceil(retryAfterMs / 1000)) }));
    },
    toast: (key, params) => toast.show(t(key, params)),
    flag: (key) => useSessionStore.getState().flags[key],
    flags: () => useSessionStore.getState().flags,
  };

  const convPort: ConvPort = {
    upsertFromWire: (payload) => useConvStore.getState().upsertFromWire(payload),
    setActive: (convId) => useConvStore.getState().setActive(convId),
  };

  const presencePort: PresencePort = {
    setStatus: (payload) => usePresenceStore.getState().setStatus(payload),
    setTyping: (convId, userId, typing, at) =>
      usePresenceStore.getState().setTyping(convId, userId, typing, at),
  };

  const messagePort: MessagePort = {
    applyReceipt: (convId, msgId, type, userId, at) =>
      useMessageStore.getState().applyReceipt(convId, msgId, type, userId, at),
  };

  const ctx: HandlerContext = {
    client: net,
    sync: engine,
    outbox,
    session: sessionPort,
    convs: convPort,
    presence: presencePort,
    messages: messagePort,
    navigate: (route) => navigate(route),
    toast: (key, params) => toast.show(t(key, params)),
  };

  net.onFrame((env) => dispatch(env, ctx, registry));
  net.onState((state, meta) => {
    useConnStore.getState().applyState(state, meta);
    outbox.handleState(state);
  });

  const authenticate = (session: AuthSession) => {
    const previousUser = useSessionStore.getState().userId;
    const storedSession = tokenStore.set(session);
    if (previousUser > 0 && previousUser !== session.userId) {
      // 切换账号：本机缓存与同步游标全部作废，避免串号
      void cache.clear().then(() => {
        useMessageStore.getState().clear();
        useConvStore.getState().clear();
        engine.reset();
        void outbox.clear();
      });
    }
    useSessionStore.getState().signIn(storedSession);
    useConvStore.getState().setSelfUserId(session.userId);
    usePresenceStore.getState().setSelfUserId(session.userId);
    navigate('/chats');
    if (net.state === 'offline') net.start();
  };

  const doLogout = async () => {
    net.stop();
    tokenStore.clear();
    useSessionStore.getState().signOut();
    useMessageStore.getState().clear();
    useConvStore.getState().clear();
    usePresenceStore.getState().clear();
    useConnStore.getState().reset();
    engine.reset();
    await outbox.clear();
    await cache.clear();
    navigate('/login');
  };

  setAppServices({
    outbox,
    engine,
    cache,
    client: net,
    authApi: IS_MOCK ? createMockAuthApi() : createHttpAuthApi(),
    authenticate,
    logout: () => {
      void doLogout();
    },
  });

  // 通话控制器的提示统一走全局 toast（net 层不直连 UI，用注入回调）。
  callController.onToast = (key) => toast.show(t(key));

  if (mock) {
    (window as Window & { __opendiscordMock?: unknown }).__opendiscordMock = {
      goOffline: () => mock.simulateOffline(),
      goOnline: () => mock.simulateOnline(),
      sendWatermark: (convId: number, latestSeq?: number) => mock.sendWatermark(convId, latestSeq),
      sendMailboxWatermark: (convId: number, mailboxSeq?: number) =>
        mock.sendMailboxWatermark(convId, mailboxSeq),
    };
  }

  if (stored) net.start();
}

function resolveWsUrl(): string {
  const explicit = import.meta.env.VITE_WS_URL;
  if (explicit) return explicit;
  if (typeof location === 'undefined') return 'ws://localhost:8080/ws';
  if (import.meta.env.DEV) {
    return `ws://${location.hostname}:8080/ws`;
  }
  const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
  return `${scheme}://${location.host}/ws`;
}
