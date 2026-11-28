/**
 * Debug log: every HTTP / WS request is written to IndexedDB with full detail.
 * Open `/debug` (hash route `#/debug`) to read all entries as plain text.
 * No secrets are stored: Authorization headers are redacted.
 */

export interface DebugEntry {
  id?: number;
  at?: number;
  kind: 'http' | 'ws-send' | 'ws-recv' | 'event' | 'error';
  direction?: 'in' | 'out';
  url?: string;
  method?: string;
  status?: number;
  headers?: Record<string, string>;
  body?: string;
  ok?: boolean;
  durationMs?: number;
  error?: string;
}

const DB_NAME = 'ufot-debug';
const STORE = 'logs';

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, 1);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE)) {
        db.createObjectStore(STORE, { keyPath: 'id', autoIncrement: true }).createIndex('at', 'at');
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

const pending: DebugEntry[] = [];
let flushTimer: ReturnType<typeof setTimeout> | null = null;

export async function appendDebug(entry: DebugEntry): Promise<void> {
  const e = { ...entry, at: entry.at ?? Date.now() };
  pending.push(e);
  if (flushTimer) return;
  flushTimer = setTimeout(async () => {
    flushTimer = null;
    const batch = pending.splice(0, pending.length);
    try {
      const db = await openDB();
      const tx = db.transaction(STORE, 'readwrite');
      const store = tx.objectStore(STORE);
      for (const item of batch) store.add(item);
      await new Promise<void>((resolve) => { tx.oncomplete = () => resolve(); tx.onerror = () => resolve(); });
      db.close();
    } catch {
      // debug log must never break the app
    }
  }, 100);
}

export async function readDebug(): Promise<DebugEntry[]> {
  try {
    const db = await openDB();
    return await new Promise((resolve) => {
      const tx = db.transaction(STORE, 'readonly');
      const req = tx.objectStore(STORE).getAll();
      req.onsuccess = () => { db.close(); resolve(req.result ?? []); };
      req.onerror = () => { db.close(); resolve([]); };
    });
  } catch {
    return [];
  }
}

export async function clearDebug(): Promise<void> {
  try {
    const db = await openDB();
    const tx = db.transaction(STORE, 'readwrite');
    tx.objectStore(STORE).clear();
    await new Promise<void>((resolve) => { tx.oncomplete = () => resolve(); tx.onerror = () => resolve(); });
    db.close();
  } catch { /* ignore */ }
}

export function debugText(entries: DebugEntry[]): string {
  return entries
    .slice()
    .sort((a, b) => (a.at ?? 0) - (b.at ?? 0))
    .map((e) => {
      const t = new Date(e.at ?? 0).toISOString();
      const head = `[${t}] ${e.kind.toUpperCase()}${e.direction ? ` ${e.direction}` : ''}`;
      const parts = [
        e.method && e.url ? `${e.method} ${e.url}` : '',
        e.status !== undefined ? `status=${e.status}` : '',
        e.ok !== undefined ? `ok=${e.ok}` : '',
        e.durationMs !== undefined ? `duration=${e.durationMs}ms` : '',
        e.error ? `error=${e.error}` : '',
        e.headers ? `headers=${JSON.stringify(e.headers)}` : '',
        e.body ? `body=${e.body}` : '',
      ].filter(Boolean);
      return `${head} ${parts.join(' | ')}`;
    })
    .join('\n');
}

export function redactHeaders(headers?: Record<string, string>): Record<string, string> | undefined {
  if (!headers) return undefined;
  return Object.fromEntries(
    Object.entries(headers).map(([k, v]) => [
      k,
      /authorization|cookie|token/i.test(k) ? '<redacted>' : v,
    ]),
  );
}

export function summarizeBody(body: unknown, max = 2000): string | undefined {
  if (body === undefined) return undefined;
  let text = '';
  if (typeof body === 'string') text = body;
  else if (body instanceof FormData) text = `[FormData] ${[...body.keys()].join(',')}`;
  else {
    try { text = JSON.stringify(body); } catch { text = String(body); }
  }
  // strip JWT-like tokens
  text = text.replace(/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/g, '<jwt>');
  return text.length > max ? `${text.slice(0, max)}…` : text;
}
