/**
 * mockBridge —— 无后端时的完整演示后端（内存 + localStorage 持久化）。
 *
 * 它实现与 WsClient 完全相同的 NetClient 接口，因此 main.tsx 只切换一行：
 *   VITE_MOCK=1 → MockBridge，否则 → WsClient。
 * 上下行仍走真实链路：Envelope → dispatch → handler → sync 引擎 → IndexedDB。
 *
 * 能力：
 *  - 启动下发 CONV_PAYLOAD / FEATURE_FLAGS / PRESENCE_UPDATE；
 *  - MSG_SEND → MSG_ACK + MSG_NEW（回显）+ 对端"正在输入" + 自动回复；
 *  - RESUME(last_seq) → RESUME_RESULT + 补拉缺失 MSG_NEW；
 *  - SYNC_REQUEST → 按 since_seq 补帧（sync 引擎 gap 触发）；
 *  - PING → PONG 心跳；
 *  - simulateOffline()/simulateOnline() 演示断网队列与重连补拉。
 *
 * 演示控制台：window.__opendiscordMock.goOffline() / goOnline()。
 */
import { createEnvelope, type Envelope } from '../proto/codec';
import {
  decodeConvCreate,
  decodeMessageSend,
  decodePing,
  decodeResumeRequest,
  decodeSyncRequest,
  encodeConvPayload,
  encodeDataResult,
  encodeFeatureFlags,
  encodeMsgAck,
  encodeMsgNew,
  encodeMsgRecall,
  encodePong,
  encodePresence,
  encodeReceipt,
  encodeResumeResult,
  encodeTyping,
  encodeWatermark,
  type ConvMember,
  type ConvPayload,
  type Media,
  type MessageContent,
  type MsgNewPayload,
} from '../proto/payload';
import { KINDS } from '../protocol/kinds';
import { AuthError, type AuthApi } from '../auth/provider';
import { mtypeToMediaType, registerMockMediaUrl, type UploadResult } from '../media';
import type { ConnState, ConnStateMeta, NetClient, ResumeTicket, StateListener } from '../transport/types';

export const MOCK_SELF_ID = 1001;
const STORE_KEY = 'opendiscord.mock.v1';

interface MockConv {
  convId: number;
  title: string;
  convType: number;
  createdAt: number;
  members: ConvMember[];
  /** 服务端会话内最新 seq（单调递增，跨消息 / 补拉） */
  serverSeq: number;
  messages: MsgNewPayload[];
}

export interface MockBridgeOptions {
  /** 首次连接延迟（ms） */
  connectDelayMs?: number;
  /** 是否持久化到 localStorage（默认真，便于刷新后 RESUME 演示） */
  persist?: boolean;
}

export class MockBridge implements NetClient {
  private _state: ConnState = 'offline';
  private readonly _latencyMs = 12;
  private started = false;
  private frameSeq = 0;
  /** v3 mailbox_seq 分配器（mock 端单写者递增，envelope.id 承载） */
  private mailboxSeq = 0;
  private convs: MockConv[] = [];
  /** mock 媒体仓库：media_id → blob（`media/{id}` 语义的内存版） */
  private mediaBlobs = new Map<string, Blob>();
  private timers = new Set<ReturnType<typeof setTimeout>>();

  private frameListeners = new Set<(env: Envelope) => void>();
  private stateListeners = new Set<StateListener>();
  private resumeProvider: (() => ResumeTicket[]) | null = null;

  private readonly connectDelayMs: number;
  private readonly persistEnabled: boolean;

  constructor(options: MockBridgeOptions = {}) {
    this.connectDelayMs = options.connectDelayMs ?? 120;
    this.persistEnabled = options.persist ?? true;
    this.convs = this.load();
  }

  get state(): ConnState {
    return this._state;
  }

  get latencyMs(): number {
    return this._latencyMs;
  }

  start(): void {
    if (this.started) return;
    this.started = true;
    this.setState('connecting');
    this.later(() => {
      this.setState('online');
      this.bootstrap();
      // 客户端在 online 后才会发 RESUME；这里等一拍再补拉
      this.later(() => this.deliverResume(), 40);
    }, this.connectDelayMs);
  }

  stop(): void {
    this.started = false;
    for (const timer of this.timers) clearTimeout(timer);
    this.timers.clear();
    this.setState('offline');
  }

  send(env: Envelope): boolean {
    if (this._state !== 'online') return false;
    switch (env.kind) {
      case KINDS.PING:
        this.handlePing(env);
        return true;
      case KINDS.RESUME:
        this.handleResume(env);
        return true;
      case KINDS.SYNC_REQUEST:
        this.handleSync(env);
        return true;
      case KINDS.MSG_SEND:
        this.handleSend(env);
        return true;
      case KINDS.CONV_CREATE:
        this.handleConvCreate(env);
        return true;
      case KINDS.MSG_RECALL:
        this.handleRecall(env);
        return true;
      case KINDS.MSG_READ:
        return true;
      case KINDS.CLIENT_CAPS:
        // §D-3：mock 不模拟能力协商 —— 不发能力帧即视为旧客户端，保持现有
        // inline/逐条 mock 投递行为不变；收到该帧仅静默吞掉（兼容真实链路形态）。
        return true;
      default:
        return true;
    }
  }

  onFrame(listener: (env: Envelope) => void): () => void {
    this.frameListeners.add(listener);
    return () => this.frameListeners.delete(listener);
  }

  onState(listener: StateListener): () => void {
    this.stateListeners.add(listener);
    return () => this.stateListeners.delete(listener);
  }

  requestHistory(convId: number, beforeSeq: number, limit: number): boolean {
    void convId; void beforeSeq; void limit;
    return false; // mock：历史全在本地，直接走 loader 的本地路径
  }

  setResumeProvider(provider: () => ResumeTicket[]): void {
    this.resumeProvider = provider;
  }

  /** 演示：断网（上行进 outbox，UI 顶部出现离线条） */
  simulateOffline(): void {
    if (this._state === 'offline') return;
    for (const timer of this.timers) clearTimeout(timer);
    this.timers.clear();
    this.setState('offline', 'simulated offline');
  }

  /** 演示：恢复网络（触发 RESUME 补拉 + outbox 重发） */
  simulateOnline(): void {
    if (!this.started) return;
    this.setState('reconnecting', 'simulated reconnect');
    this.later(() => {
      this.setState('online');
      this.bootstrap();
      this.later(() => this.deliverResume(), 60);
    }, 400);
  }

  /** mock 媒体读取（`media/{id}` 语义的内存版）。 */
  mediaBlob(mediaId: string): Blob | null {
    return this.mediaBlobs.get(mediaId) ?? null;
  }

  /**
   * 模拟服务端推 WATERMARK(14)（spec v1.2 §2.3 mock 注入点，供单测与演示）。
   * latestSeq 缺省取该会话当前 serverSeq。经 emit 走真实 dispatch → handler 链路。
   */
  sendWatermark(convId: number, latestSeq?: number, reason = 0): boolean {
    const conv = this.convs.find((candidate) => candidate.convId === convId);
    if (!conv) return false;
    const seq = latestSeq ?? conv.serverSeq;
    this.emit(KINDS.WATERMARK, {
      convId,
      payload: encodeWatermark({ convId, latestSeq: seq, reason, memberCount: conv.members.length }),
    });
    return true;
  }

  /**
   * v3 mailbox 水线注入（docs/v3-bus-design.md §2/§4，mock 端到端用）。
   * v3 语义下服务端下行帧的 envelope.id 即 mailbox_seq（单写者递增）；本方法
   * 以自增 mailboxSeq 发 WATERMARK(14) 指针帧（大群水位线形态），走真实
   * dispatch → handler 链路，供 MailboxSync 游标/ACK 状态机的 mock 验证。
   */
  sendMailboxWatermark(convId: number, mailboxSeq?: number): boolean {
    const conv = this.convs.find((candidate) => candidate.convId === convId);
    if (!conv) return false;
    if (mailboxSeq === undefined) this.mailboxSeq += 1;
    else this.mailboxSeq = Math.max(this.mailboxSeq, mailboxSeq);
    this.emit(KINDS.WATERMARK, {
      id: this.mailboxSeq,
      convId,
      payload: encodeWatermark({
        convId,
        latestSeq: conv.serverSeq,
        reason: 0,
        memberCount: conv.members.length,
      }),
    });
    return true;
  }

  /**
   * mock 媒体上传：内存存 blob → media_id（`m_up_` 前缀），并注册 blob URL
   * 供 renderers 回显（net/media.resolveMediaUrl 在 mock 模式下查询）。
   * 规则与服务端 mediasvc 同源：blocked 扩展名拒绝、大小上限一致。
   */
  async uploadMedia(file: File, onProgress?: (ratio: number) => void): Promise<UploadResult> {
    const { classifyUpload, mtypeToMediaType } = await import('../media');
    const classified = await classifyUpload(file);
    // 模拟分片上传进度
    for (let ratio = 0.25; ratio < 1; ratio += 0.25) {
      await delay(90);
      onProgress?.(ratio);
    }
    await delay(90);
    onProgress?.(1);
    const mediaId = `m_up_${Date.now()}_${Math.abs(hash(file.name + file.size))}`;
    this.mediaBlobs.set(mediaId, file.slice(0));
    registerMockMediaUrl(mediaId, URL.createObjectURL(file));
    return {
      mediaId,
      size: file.size,
      mime: classified.mime,
      mtype: classified.mtype,
    };
  }

  /** mock 上传结果 → 消息 Media 字段（图片宽高由调用方从 Image 补齐）。 */
  toMedia(result: UploadResult, extra: Partial<Media> = {}): Media {
    return {
      mediaId: result.mediaId,
      type: mtypeToMediaType(result.mtype),
      name: extra.name,
      size: result.size,
      mime: result.mime,
      width: extra.width,
      height: extra.height,
      status: 'ready',
    };
  }

  // -------------------------------------------------------------------------
  // 下行帧
  // -------------------------------------------------------------------------

  private handlePing(env: Envelope): void {
    let clientMs = 0;
    try {
      clientMs = decodePing(env.payload).clientMs;
    } catch {
      /* ignore */
    }
    this.later(() => {
      this.emit(KINDS.PONG, { payload: encodePong(clientMs, Date.now()) });
    }, 20);
  }

  private handleResume(env: Envelope): void {
    let request: { convId: number; lastSeq: number };
    try {
      request = decodeResumeRequest(env.payload);
    } catch {
      return;
    }
    const conv = this.convs.find((candidate) => candidate.convId === request.convId);
    if (!conv) return;
    // 服务端 seq 不得落后于客户端（mock 重启/清库时以客户端游标为准）
    conv.serverSeq = Math.max(conv.serverSeq, request.lastSeq);
    this.emit(KINDS.RESUME_RESULT, {
      convId: conv.convId,
      payload: encodeResumeResult({
        convId: conv.convId,
        fromSeq: request.lastSeq + 1,
        complete: true,
        serverSeq: conv.serverSeq,
      }),
    });
    this.replay(conv, request.lastSeq + 1, 200);
  }

  private handleSync(env: Envelope): void {
    let request: { convId: number; sinceSeq: number; limit: number };
    try {
      request = decodeSyncRequest(env.payload);
    } catch {
      return;
    }
    const conv = this.convs.find((candidate) => candidate.convId === request.convId);
    if (!conv) return;
    conv.serverSeq = Math.max(conv.serverSeq, request.sinceSeq - 1);
    this.replay(conv, request.sinceSeq, request.limit || 100);
  }

  private replay(conv: MockConv, sinceSeq: number, limit: number): void {
    const missing = conv.messages.filter((message) => message.seq >= sinceSeq).slice(0, limit);
    missing.forEach((message, index) => {
      this.later(() => this.emitMsgNew(message), 10 + index * 4);
    });
  }

  /**
   * CONV_CREATE(200)：与真实服务端同语义 —— 单聊去重、群聊必须带群名。
   * 成功后回 931（body 为完整 ConvPayload，含新 cid），由 931 handler
   * upsert conv-store 并打开会话。
   */
  private handleConvCreate(env: Envelope): void {
    let input: { title?: string; convType: number; memberIds: number[] };
    try {
      input = decodeConvCreate(env.payload);
    } catch {
      return;
    }
    const target = input.memberIds[0] ?? 0;
    if (!target) {
      this.emit(KINDS.CONV_DATA_RESULT, {
        requestId: env.requestId,
        payload: encodeDataResult({ op: 1, ok: false, error: 'target user required' }),
      });
      return;
    }
    const convType = input.convType === 2 ? 2 : 1;
    let conv: MockConv | undefined;
    if (convType === 1) {
      conv = this.convs.find(
        (candidate) =>
          candidate.convType === 1 &&
          candidate.members.length === 2 &&
          candidate.members.some((m) => m.userId === MOCK_SELF_ID) &&
          candidate.members.some((m) => m.userId === target),
      );
    }
    if (!conv) {
      const nextId = this.convs.reduce((max, candidate) => Math.max(max, candidate.convId), 1000) + 1;
      conv = {
        convId: nextId,
        title: convType === 2 ? input.title?.trim() || '新群聊' : `用户 ${target}`,
        convType,
        createdAt: Date.now(),
        serverSeq: 0,
        members: [
          { userId: MOCK_SELF_ID, role: 2, joinedAt: Date.now(), mutedUntil: 0 },
          { userId: target, role: 0, joinedAt: Date.now(), mutedUntil: 0 },
        ],
        messages: [],
      };
      this.convs.push(conv);
      this.persist();
    }
    const payload: ConvPayload = {
      convId: conv.convId,
      title: conv.title,
      convType: conv.convType,
      createdAt: conv.createdAt,
      members: conv.members,
      version: 1,
      lastSeq: conv.serverSeq,
      unread: 0,
    };
    this.later(() => {
      this.emit(KINDS.CONV_DATA_RESULT, {
        requestId: env.requestId,
        payload: encodeDataResult({ op: 1, ok: true, body: encodeConvPayload(payload) }),
      });
      this.emitConv(conv!);
    }, 40);
  }

  private handleSend(env: Envelope): void {
    let content: MessageContent;
    try {
      content = decodeMessageSend(env.payload);
    } catch {
      return;
    }
    const convId = env.convId ?? 0;
    const conv = this.convs.find((candidate) => candidate.convId === convId);
    if (!conv) return;

    const seq = conv.serverSeq + 1;
    conv.serverSeq = seq;
    const message: MsgNewPayload = {
      msgId: nextMsgId(convId, seq),
      seq,
      senderId: MOCK_SELF_ID,
      sentAt: Date.now(),
      convId,
      content,
    };
    conv.messages.push(message);
    this.trim(conv);
    this.persist();

    this.later(() => {
      this.emit(KINDS.MSG_ACK, {
        convId,
        payload: encodeMsgAck({
          msgId: message.msgId,
          seq: message.seq,
          clientMsgId: content.clientMsgId,
          status: 0,
        }),
      });
    }, 60);

    this.later(() => this.emitMsgNew(message), 120);

    // 对端已读回执：让发送者看到双勾
    this.later(() => {
      this.emit(KINDS.MSG_READ, {
        convId,
        payload: encodeReceipt({ msgId: message.msgId, userId: peerOf(conv), at: Date.now(), type: 'read' }),
      });
    }, 900);

    this.scheduleBotReply(conv, content.text ?? '');
  }

  /**
   * MSG_RECALL(108) 上行回环（对齐服务端 onRecall 语义）：
   * 定位消息 → 标记撤回 → 向"全员"（含发送者）广播 108 {msg_id, conv_id}，
   * 客户端 msg-mutation handler 映射为 recall tombstone。
   */
  private handleRecall(env: Envelope): void {
    let msgId = 0;
    let convId = env.convId ?? 0;
    try {
      const raw = JSON.parse(new TextDecoder().decode(env.payload)) as Record<string, unknown>;
      msgId = Number(raw.msg_id) || 0;
      if (!convId) convId = Number(raw.conv_id) || 0;
    } catch {
      /* bad payload: 忽略 */
    }
    if (!msgId || !convId) return;
    const conv = this.convs.find((candidate) => candidate.convId === convId);
    const message = conv?.messages.find((candidate) => candidate.msgId === msgId);
    if (!conv || !message) return;
    if (message.senderId !== MOCK_SELF_ID) return; // 简化：mock 中本人非管理员
    if (Date.now() - message.sentAt > 120_000) return; // 2 分钟撤回窗口
    message.recalled = Math.floor(Date.now() / 1000);
    this.persist();
    this.later(() => {
      this.emit(KINDS.MSG_RECALL, {
        convId,
        payload: encodeMsgRecall({ msgId, convId }),
      });
    }, 60);
  }

  private scheduleBotReply(conv: MockConv, text: string): void {
    const peer = conv.members.find((member) => member.userId !== MOCK_SELF_ID);
    if (!peer) return;
    const delay = 900 + (conv.serverSeq % 4) * 220;

    this.later(() => {
      this.emit(KINDS.TYPING, {
        convId: conv.convId,
        payload: encodeTyping(conv.convId, peer.userId, true),
      });
    }, 320);

    this.later(() => {
      this.emit(KINDS.TYPING, {
        convId: conv.convId,
        payload: encodeTyping(conv.convId, peer.userId, false),
      });
      const seq = conv.serverSeq + 1;
      conv.serverSeq = seq;
      const reply: MsgNewPayload = {
        msgId: nextMsgId(conv.convId, seq),
        seq,
        senderId: peer.userId,
        sentAt: Date.now(),
        convId: conv.convId,
        content: {
          clientMsgId: '',
          text: pickReply(conv.convId, text),
          media: [],
          mentions: [],
        },
      };
      conv.messages.push(reply);
      this.trim(conv);
      this.persist();
      this.emitMsgNew(reply);
    }, delay);
  }

  private bootstrap(): void {
    for (const conv of this.convs) this.emitConv(conv);
    this.emit(KINDS.FEATURE_FLAGS, {
      payload: encodeFeatureFlags(
        { 'media.enabled': 'true', 'read.receipt': 'true', 'chat.max_length': '500', 'call_enabled': 'true' },
        1,
      ),
    });
    this.emit(KINDS.PRESENCE_UPDATE, { payload: encodePresence(2001, 'online') });
    this.emit(KINDS.PRESENCE_UPDATE, { payload: encodePresence(2002, 'away') });
  }

  private deliverResume(): void {
    const tickets = this.resumeProvider?.() ?? [];
    for (const ticket of tickets) {
      const conv = this.convs.find((candidate) => candidate.convId === ticket.convId);
      if (!conv) continue;
      conv.serverSeq = Math.max(conv.serverSeq, ticket.lastSeq);
      this.emit(KINDS.RESUME_RESULT, {
        convId: conv.convId,
        payload: encodeResumeResult({
          convId: conv.convId,
          fromSeq: ticket.lastSeq + 1,
          complete: true,
          serverSeq: conv.serverSeq,
        }),
      });
      this.replay(conv, ticket.lastSeq + 1, 200);
    }
  }

  private emitConv(conv: MockConv): void {
    const payload: ConvPayload = {
      convId: conv.convId,
      title: conv.title,
      convType: conv.convType,
      createdAt: conv.createdAt,
      members: conv.members,
      version: 1,
      lastSeq: conv.serverSeq,
      unread: 0,
    };
    this.emit(KINDS.CONV_PAYLOAD, { convId: conv.convId, payload: encodeConvPayload(payload) });
  }

  private emitMsgNew(message: MsgNewPayload): void {
    this.emit(KINDS.MSG_NEW, {
      convId: message.convId,
      payload: encodeMsgNew(message),
    });
  }

  private emit(
    kind: number,
    options: { id?: number; convId?: number; userId?: number; payload: Uint8Array; requestId?: string },
  ): void {
    if (this._state !== 'online') return;
    const env = createEnvelope({
      id: options.id ?? ++this.frameSeq,
      kind,
      requestId: options.requestId,
      convId: options.convId,
      userId: options.userId,
      sentAt: Date.now(),
      payload: options.payload,
    });
    for (const listener of this.frameListeners) listener(env);
  }

  private setState(state: ConnState, reason?: string): void {
    if (this._state === state) return;
    this._state = state;
    const meta: ConnStateMeta = {
      attempt: state === 'reconnecting' ? 1 : 0,
      reason,
      latencyMs: this._latencyMs,
      at: Date.now(),
    };
    for (const listener of this.stateListeners) listener(state, meta);
  }

  private later(fn: () => void, ms: number): void {
    const timer = setTimeout(() => {
      this.timers.delete(timer);
      fn();
    }, ms);
    this.timers.add(timer);
  }

  private trim(conv: MockConv): void {
    if (conv.messages.length > 500) conv.messages.splice(0, conv.messages.length - 500);
  }

  private load(): MockConv[] {
    if (this.persistEnabled && typeof localStorage !== 'undefined') {
      try {
        const raw = localStorage.getItem(STORE_KEY);
        if (raw) {
          const parsed = JSON.parse(raw) as MockConv[];
          if (Array.isArray(parsed) && parsed.length > 0) return parsed;
        }
      } catch {
        /* 损坏则重新播种 */
      }
    }
    const seeded = seed();
    this.convs = seeded;
    this.persist();
    return seeded;
  }

  private persist(): void {
    if (!this.persistEnabled || typeof localStorage === 'undefined') return;
    try {
      localStorage.setItem(STORE_KEY, JSON.stringify(this.convs));
    } catch {
      /* 配额满：mock 仍可运行 */
    }
  }
}

// ---------------------------------------------------------------------------
// 种子数据：文本 / 图片 / 语音 / 文件 / 系统消息，覆盖全部 renderer
// ---------------------------------------------------------------------------

function seed(): MockConv[] {
  const now = Date.now();
  const image = demoImage(now);
  const audio = demoAudio();
  const file = demoFile();

  const product: MockConv = {
    convId: 1,
    title: '产品讨论组',
    convType: 2,
    createdAt: now - 7 * 86400_000,
    serverSeq: 0,
    members: [
      { userId: MOCK_SELF_ID, role: 1, joinedAt: now - 7 * 86400_000, mutedUntil: 0, name: '我' },
      { userId: 2001, role: 2, joinedAt: now - 6 * 86400_000, mutedUntil: 0, name: '林小雨' },
      { userId: 2002, role: 0, joinedAt: now - 5 * 86400_000, mutedUntil: 0, name: '陈一' },
    ],
    messages: [],
  };
  push(product, 2001, { text: '早，今天把消息列表的虚拟滚动接一下？' }, now - 3 * 3600_000);
  push(product, MOCK_SELF_ID, { text: '已经在做了，先把 seq 收敛引擎跑通。' }, now - 2.9 * 3600_000);
  push(product, 2002, { media: [image], text: '' }, now - 2.8 * 3600_000);
  push(product, 2001, { text: '图里是新的气泡间距，确认一下 4px 网格。' }, now - 2.7 * 3600_000);
  push(product, 2001, { media: [audio] }, now - 2.6 * 3600_000);
  push(product, MOCK_SELF_ID, { text: '收到，晚上发版前对齐。' }, now - 2.5 * 3600_000);
  push(product, 2001, { media: [file], text: '' }, now - 2.4 * 3600_000);

  const direct: MockConv = {
    convId: 2,
    title: '林小雨',
    convType: 1,
    createdAt: now - 30 * 86400_000,
    serverSeq: 0,
    members: [
      { userId: MOCK_SELF_ID, role: 0, joinedAt: now - 30 * 86400_000, mutedUntil: 0, name: '我' },
      { userId: 2001, role: 0, joinedAt: now - 30 * 86400_000, mutedUntil: 0, name: '林小雨' },
    ],
    messages: [],
  };
  push(direct, 2001, { text: '在吗？' }, now - 42 * 60_000);
  push(direct, MOCK_SELF_ID, { text: '在的，怎么了' }, now - 41 * 60_000);
  push(direct, 2001, { text: '周末一起去看展？票我已经买好了。' }, now - 40 * 60_000);
  push(direct, 0, { text: '你已添加林小雨为好友，现在可以开始聊天了' }, now - 39 * 60_000);

  return [product, direct];
}

function push(
  conv: MockConv,
  senderId: number,
  content: Partial<MessageContent>,
  at: number,
): MsgNewPayload {
  const seq = conv.serverSeq + 1;
  conv.serverSeq = seq;
  const message: MsgNewPayload = {
    msgId: nextMsgId(conv.convId, seq),
    seq,
    senderId,
    sentAt: at,
    convId: conv.convId,
    content: {
      clientMsgId: '',
      text: content.text ?? '',
      media: content.media ?? [],
      mentions: content.mentions ?? [],
      replyTo: content.replyTo,
    },
  };
  conv.messages.push(message);
  return message;
}

function demoImage(now: number): Media {
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="800" height="600">
    <defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#3B5BFD"/><stop offset="1" stop-color="#8B9DF9"/>
    </linearGradient></defs>
    <rect width="800" height="600" fill="url(#g)"/>
    <circle cx="620" cy="140" r="90" fill="#fff" opacity="0.18"/>
    <text x="48" y="300" fill="#fff" font-size="52" font-family="PingFang SC,sans-serif">新消息气泡 · 间距规范</text>
    <text x="48" y="370" fill="#DCE2FF" font-size="28" font-family="PingFang SC,sans-serif">4px 网格 / 14px 外角 / 4px 内角</text>
  </svg>`;
  const url = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
  return {
    mediaId: `m_img_${now}`,
    type: 'image',
    name: '气泡规范.png',
    size: 182_400,
    width: 800,
    height: 600,
    mime: 'image/svg+xml',
    thumb: url,
    url,
    status: 'ready',
  };
}

function demoAudio(): Media {
  return {
    mediaId: `m_audio_${Date.now()}`,
    type: 'audio',
    name: '语音 12".m4a',
    size: 48_200,
    durationMs: 12_400,
    mime: 'audio/mp4',
    waveform: [20, 45, 80, 35, 60, 95, 50, 30, 70, 90, 40, 25, 55, 85, 65, 30, 75, 95, 45, 20, 60, 80, 40, 30],
    status: 'ready',
  };
}

function demoFile(): Media {
  return {
    mediaId: `m_file_${Date.now()}`,
    type: 'file',
    name: '项目规划书.pdf',
    size: 2_400_000,
    mime: 'application/pdf',
    status: 'ready',
  };
}

/** net/media.ts 在 mock 模式下调用的入口（避免 media.ts ↔ mockBridge 顶层循环依赖）。 */
export async function mockUploadMedia(
  file: File,
  onProgress?: (ratio: number) => void,
): Promise<UploadResult> {
  const bridge = getActiveBridge();
  if (!bridge) throw new Error('mock upload before bridge init');
  return bridge.uploadMedia(file, onProgress);
}

/** 当前活跃 bridge（模块级单例，bootstrap 创建时注册）。 */
let activeBridge: MockBridge | null = null;
export function setActiveBridge(bridge: MockBridge | null): void {
  activeBridge = bridge;
}
function getActiveBridge(): MockBridge | null {
  return activeBridge;
}

function nextMsgId(convId: number, seq: number): number {
  return 1_700_000_000_000 + convId * 100_000 + seq;
}

function peerOf(conv: MockConv): number {
  return conv.members.find((member) => member.userId !== MOCK_SELF_ID)?.userId ?? 0;
}

const REPLIES: Record<number, string[]> = {
  1: ['这个方案可以，先把同步链路压测一遍。', '收到，我下午把设计稿更新到最新版。', '好，那就按 4px 网格统一。', '我看了下截图，气泡最大宽度还要再收敛一档。'],
  2: ['好呀，那我们周六下午见。', '哈哈哈可以，票我留着。', '刚看到消息，稍等我回你。', '嗯嗯，那就这么定了。'],
};

function pickReply(convId: number, text: string): string {
  if (/[?？]/.test(text)) return '我想一下，稍后回复你。';
  const list = REPLIES[convId] ?? ['收到。'];
  return list[Math.abs(hash(text + convId)) % list.length];
}

function hash(value: string): number {
  let h = 0;
  for (let i = 0; i < value.length; i += 1) h = (h * 31 + value.charCodeAt(i)) | 0;
  return h;
}

// ---------------------------------------------------------------------------
// mock 登录 API：与真实后端同路径、同 snake_case 应答
// ---------------------------------------------------------------------------

export function createMockAuthApi(): AuthApi {
  const failures = new Map<string, number>();
  return {
    async post<T>(path: string, body: unknown): Promise<T> {
      await delay(180);
      const input = (body ?? {}) as Record<string, unknown>;
      switch (path) {
        case '/api/auth/phone/code': {
          const phone = String(input.phone ?? '');
          if (!/^1[3-9]\d{9}$/.test(phone) && !/^\+\d{6,15}$/.test(phone)) {
            throw new AuthError('手机号格式不正确', 'INVALID_PHONE');
          }
          return {
            challenge_id: `otp_${Date.now()}`,
            channel: 'sms',
            expires_in: 300,
            resend_after: 60,
            hint: '演示环境验证码：123456',
          } as T;
        }
        case '/api/auth/phone/verify': {
          if (String(input.code ?? '') !== '123456') {
            throw new AuthError('验证码错误或已过期', 'CODE_INVALID');
          }
          return session('我', false) as T;
        }
        case '/api/auth/email/link':
          return {
            challenge_id: `magic_${Date.now()}`,
            channel: 'email',
            expires_in: 600,
            resend_after: 60,
            hint: '演示环境点击「我已点击链接」即可登录',
          } as T;
        case '/api/auth/email/poll':
          return session('我', false) as T;
        case '/api/auth/password': {
          const account = String(input.account ?? '');
          const password = String(input.password ?? '');
          const count = failures.get(account) ?? 0;
          if (count >= 5 && !String(input.captcha ?? '').trim()) {
            throw new AuthError('请输入图形验证码', 'CAPTCHA_REQUIRED');
          }
          if (password !== 'opendiscord' && count < 5) {
            failures.set(account, count + 1);
            throw new AuthError('账号或密码错误', 'BAD_CREDENTIALS');
          }
          failures.delete(account);
          return session(account || '我', false) as T;
        }
        case '/api/auth/guest':
          return session(String(input.nickname ?? '游客') || '游客', true) as T;
        default:
          throw new AuthError(`unknown auth endpoint ${path}`, 'NOT_FOUND');
      }
    },
  };
}

/**
 * mock 登录会话 —— 与 authsvc sessionResponse（server/internal/authsvc/
 * tokens.go:27）逐字段同形（snake_case），sessionId = deviceId（tokens.go:74
 * SessionID: deviceID，真实端同语义）。返回值经 mapAuthSession 映射为前端
 * AuthSession 领域对象，与真实端走同一条路径。
 */
export interface RawSessionResponse {
  user_id: number;
  device_id: string;
  access_token: string;
  /** tokens.go:31 "alias kept for older clients" —— 保留以锁死契约形状 */
  token: string;
  refresh_token: string;
  expires_at: number;
  expires_at_unix: number;
  session_id: string;
  guest: boolean;
  is_new_user: boolean;
  profile?: { display_name?: string };
}

function session(displayName: string, guest: boolean): RawSessionResponse {
  const now = Date.now();
  const deviceId = `mock-device-${Math.floor(now / 1000)}`;
  const expiresAt = now + 7 * 86400_000;
  return {
    user_id: MOCK_SELF_ID,
    device_id: deviceId,
    access_token: `mock-token-${now}`,
    token: `mock-token-${now}`, // tokens.go:31 alias kept for older clients
    refresh_token: `mock-refresh-${now}`,
    expires_at: expiresAt, // 毫秒（与真实端一致）
    expires_at_unix: Math.floor(expiresAt / 1000),
    session_id: deviceId,
    guest,
    is_new_user: true, // mock 首登即新用户，真实端由 authsvc 判定
    profile: displayName ? { display_name: displayName } : undefined,
  };
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
