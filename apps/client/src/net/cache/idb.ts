/**
 * IndexedDB 三库：messages / convs / mediaUrls。
 *
 * 设计约束（见 docs/user-frontend-design.md §2.3）：
 *  - IndexedDB 是渲染唯一事实源，内存 store 只是投影；
 *  - 只有 sync 引擎写（single-writer），UI 只读；
 *  - 离线启动 hydrate：UI 先从 IndexedDB 渲染，连接建立后增量收敛。
 *
 * 所有方法失败都不应让应用白屏：`openCache()` 在 IndexedDB 不可用
 * （隐私模式 / 无痕 / 测试环境）时自动降级为同语义的 MemoryCache。
 */
import type {
  ConvRecord,
  HydratedCache,
  StoredMessage,
} from '../sync/types';

export interface SyncCache {
  hydrate(limitPerConv?: number): Promise<HydratedCache>;
  getConv(convId: number): Promise<ConvRecord | null>;
  putConv(conv: ConvRecord): Promise<void>;
  putConvs(convs: ConvRecord[]): Promise<void>;
  getLastSeq(convId: number): Promise<number>;
  saveLastSeq(convId: number, lastSeq: number): Promise<void>;
  putMessages(messages: StoredMessage[]): Promise<void>;
  /** 按 seq 升序返回；beforeSeq 用于「加载更早」分页 */
  listMessages(convId: number, limit: number, beforeSeq?: number): Promise<StoredMessage[]>;
  getMessageByMsgId(convId: number, msgId: number): Promise<StoredMessage | null>;
  putMediaUrl(mediaId: string, url: string, ttlMs?: number): Promise<void>;
  getMediaUrl(mediaId: string): Promise<string | null>;
  clear(): Promise<void>;
}

const DB_NAME = 'ufot';
const DB_VERSION = 1;
const STORE_MESSAGES = 'messages';
const STORE_CONVS = 'convs';
const STORE_MEDIA = 'mediaUrls';
const DEFAULT_MEDIA_TTL_MS = 24 * 60 * 60 * 1000;

interface MessageRow extends StoredMessage {
  /** `${convId}:${seq}` —— IDB 主键 */
  id: string;
}

interface MediaRow {
  mediaId: string;
  url: string;
  expiresAt: number;
}

function messageRow(message: StoredMessage): MessageRow {
  return { ...message, id: `${message.convId}:${message.seq}` };
}

function requestToPromise<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('IndexedDB request failed'));
  });
}

function transactionDone(tx: IDBTransaction): Promise<void> {
  return new Promise((resolve, reject) => {
    tx.oncomplete = () => resolve();
    tx.onerror = () => reject(tx.error ?? new Error('IndexedDB transaction failed'));
    tx.onabort = () => reject(tx.error ?? new Error('IndexedDB transaction aborted'));
  });
}

export class IdbCache implements SyncCache {
  private openPromise: Promise<IDBDatabase> | null = null;

  ready(): Promise<void> {
    return this.open().then(() => undefined);
  }

  private open(): Promise<IDBDatabase> {
    if (this.openPromise) return this.openPromise;
    this.openPromise = new Promise((resolve, reject) => {
      const request = indexedDB.open(DB_NAME, DB_VERSION);
      request.onupgradeneeded = () => {
        const db = request.result;
        if (!db.objectStoreNames.contains(STORE_MESSAGES)) {
          const store = db.createObjectStore(STORE_MESSAGES, { keyPath: 'id' });
          store.createIndex('by_conv_seq', ['convId', 'seq'], { unique: true });
          store.createIndex('by_msg', ['convId', 'msgId'], { unique: false });
        }
        if (!db.objectStoreNames.contains(STORE_CONVS)) {
          db.createObjectStore(STORE_CONVS, { keyPath: 'convId' });
        }
        if (!db.objectStoreNames.contains(STORE_MEDIA)) {
          db.createObjectStore(STORE_MEDIA, { keyPath: 'mediaId' });
        }
      };
      request.onsuccess = () => resolve(request.result);
      request.onerror = () => reject(request.error ?? new Error('cannot open IndexedDB'));
      request.onblocked = () => reject(new Error('IndexedDB upgrade blocked'));
    });
    return this.openPromise;
  }

  async hydrate(limitPerConv = 60): Promise<HydratedCache> {
    const db = await this.open();
    const tx = db.transaction(STORE_CONVS, 'readonly');
    const convs = (await requestToPromise(tx.objectStore(STORE_CONVS).getAll())) as ConvRecord[];
    convs.sort((a, b) => (b.updatedAt ?? 0) - (a.updatedAt ?? 0));
    const groups = await Promise.all(
      convs.map((conv) => this.listMessages(conv.convId, limitPerConv)),
    );
    return { convs, messages: groups.flat() };
  }

  async getConv(convId: number): Promise<ConvRecord | null> {
    const db = await this.open();
    const tx = db.transaction(STORE_CONVS, 'readonly');
    const row = await requestToPromise(tx.objectStore(STORE_CONVS).get(convId));
    return (row as ConvRecord | undefined) ?? null;
  }

  async putConv(conv: ConvRecord): Promise<void> {
    const db = await this.open();
    const tx = db.transaction(STORE_CONVS, 'readwrite');
    tx.objectStore(STORE_CONVS).put(conv);
    await transactionDone(tx);
  }

  async putConvs(convs: ConvRecord[]): Promise<void> {
    if (convs.length === 0) return;
    const db = await this.open();
    const tx = db.transaction(STORE_CONVS, 'readwrite');
    const store = tx.objectStore(STORE_CONVS);
    for (const conv of convs) store.put(conv);
    await transactionDone(tx);
  }

  async getLastSeq(convId: number): Promise<number> {
    const conv = await this.getConv(convId);
    return conv?.lastSeq ?? 0;
  }

  async saveLastSeq(convId: number, lastSeq: number): Promise<void> {
    const existing = await this.getConv(convId);
    const now = Date.now();
    const next: ConvRecord = existing ?? {
      convId,
      title: '',
      convType: 0,
      createdAt: now,
      members: [],
      version: 0,
      lastSeq: 0,
      unread: 0,
      updatedAt: now,
    };
    next.lastSeq = lastSeq;
    next.updatedAt = now;
    await this.putConv(next);
  }

  async putMessages(messages: StoredMessage[]): Promise<void> {
    if (messages.length === 0) return;
    const db = await this.open();
    const tx = db.transaction(STORE_MESSAGES, 'readwrite');
    const store = tx.objectStore(STORE_MESSAGES);
    for (const message of messages) store.put(messageRow(message));
    await transactionDone(tx);
  }

  async listMessages(convId: number, limit: number, beforeSeq?: number): Promise<StoredMessage[]> {
    const db = await this.open();
    const tx = db.transaction(STORE_MESSAGES, 'readonly');
    const index = tx.objectStore(STORE_MESSAGES).index('by_conv_seq');
    const upper = beforeSeq === undefined ? [convId, Number.MAX_SAFE_INTEGER] : [convId, beforeSeq - 1];
    const range = IDBKeyRange.bound([convId, 0], upper);
    const rows: StoredMessage[] = [];
    await new Promise<void>((resolve, reject) => {
      const request = index.openCursor(range, 'prev');
      request.onsuccess = () => {
        const cursor = request.result;
        if (!cursor || rows.length >= limit) {
          resolve();
          return;
        }
        const { id: _id, ...message } = cursor.value as MessageRow;
        rows.push(message);
        cursor.continue();
      };
      request.onerror = () => reject(request.error);
    });
    return rows.reverse();
  }

  async getMessageByMsgId(convId: number, msgId: number): Promise<StoredMessage | null> {
    const db = await this.open();
    const tx = db.transaction(STORE_MESSAGES, 'readonly');
    const index = tx.objectStore(STORE_MESSAGES).index('by_msg');
    const row = (await requestToPromise(index.get([convId, msgId]))) as MessageRow | undefined;
    if (!row) return null;
    const { id: _id, ...message } = row;
    return message;
  }

  async putMediaUrl(mediaId: string, url: string, ttlMs = DEFAULT_MEDIA_TTL_MS): Promise<void> {
    const db = await this.open();
    const tx = db.transaction(STORE_MEDIA, 'readwrite');
    tx.objectStore(STORE_MEDIA).put({ mediaId, url, expiresAt: Date.now() + ttlMs } satisfies MediaRow);
    await transactionDone(tx);
  }

  async getMediaUrl(mediaId: string): Promise<string | null> {
    const db = await this.open();
    const tx = db.transaction(STORE_MEDIA, 'readonly');
    const row = (await requestToPromise(tx.objectStore(STORE_MEDIA).get(mediaId))) as MediaRow | undefined;
    if (!row) return null;
    if (row.expiresAt > 0 && row.expiresAt < Date.now()) return null;
    return row.url;
  }

  async clear(): Promise<void> {
    const db = await this.open();
    const tx = db.transaction([STORE_MESSAGES, STORE_CONVS, STORE_MEDIA], 'readwrite');
    tx.objectStore(STORE_MESSAGES).clear();
    tx.objectStore(STORE_CONVS).clear();
    tx.objectStore(STORE_MEDIA).clear();
    await transactionDone(tx);
  }
}

/** 无 IndexedDB 时的同语义内存实现（测试 / 隐私模式降级）。 */
export class MemoryCache implements SyncCache {
  private convs = new Map<number, ConvRecord>();
  private messages = new Map<number, StoredMessage[]>();
  private media = new Map<string, MediaRow>();

  async hydrate(limitPerConv = 60): Promise<HydratedCache> {
    const convs = [...this.convs.values()].sort((a, b) => b.updatedAt - a.updatedAt);
    const messages = convs.flatMap((conv) => (this.messages.get(conv.convId) ?? []).slice(-limitPerConv));
    return { convs, messages };
  }

  async getConv(convId: number): Promise<ConvRecord | null> {
    return this.convs.get(convId) ?? null;
  }

  async putConv(conv: ConvRecord): Promise<void> {
    this.convs.set(conv.convId, { ...conv });
  }

  async putConvs(convs: ConvRecord[]): Promise<void> {
    for (const conv of convs) this.convs.set(conv.convId, { ...conv });
  }

  async getLastSeq(convId: number): Promise<number> {
    return this.convs.get(convId)?.lastSeq ?? 0;
  }

  async saveLastSeq(convId: number, lastSeq: number): Promise<void> {
    const existing = this.convs.get(convId);
    const now = Date.now();
    this.convs.set(convId, {
      ...(existing ?? {
        convId,
        title: '',
        convType: 0,
        createdAt: now,
        members: [],
        version: 0,
        unread: 0,
      }),
      lastSeq,
      updatedAt: now,
    });
  }

  async putMessages(messages: StoredMessage[]): Promise<void> {
    for (const message of messages) {
      const list = this.messages.get(message.convId) ?? [];
      const index = list.findIndex((m) => m.seq === message.seq);
      if (index >= 0) list[index] = { ...message };
      else list.push({ ...message });
      list.sort((a, b) => a.seq - b.seq);
      this.messages.set(message.convId, list);
    }
  }

  async listMessages(convId: number, limit: number, beforeSeq?: number): Promise<StoredMessage[]> {
    const list = this.messages.get(convId) ?? [];
    const filtered = beforeSeq === undefined ? list : list.filter((m) => m.seq < beforeSeq);
    return filtered.slice(-limit).map((m) => ({ ...m }));
  }

  async getMessageByMsgId(convId: number, msgId: number): Promise<StoredMessage | null> {
    const found = (this.messages.get(convId) ?? []).find((m) => m.msgId === msgId);
    return found ? { ...found } : null;
  }

  async putMediaUrl(mediaId: string, url: string, ttlMs = DEFAULT_MEDIA_TTL_MS): Promise<void> {
    this.media.set(mediaId, { mediaId, url, expiresAt: Date.now() + ttlMs });
  }

  async getMediaUrl(mediaId: string): Promise<string | null> {
    const row = this.media.get(mediaId);
    if (!row) return null;
    if (row.expiresAt > 0 && row.expiresAt < Date.now()) return null;
    return row.url;
  }

  async clear(): Promise<void> {
    this.convs.clear();
    this.messages.clear();
    this.media.clear();
  }
}

/** 工厂：优先 IndexedDB，不可用/打开失败时降级内存，保证离线首屏不白屏。 */
export async function openCache(): Promise<SyncCache> {
  if (typeof indexedDB === 'undefined') return new MemoryCache();
  try {
    const cache = new IdbCache();
    await cache.ready();
    return cache;
  } catch (error) {
    console.warn('[cache] IndexedDB unavailable, falling back to memory', error);
    return new MemoryCache();
  }
}

export function createMemoryCache(): SyncCache {
  return new MemoryCache();
}
