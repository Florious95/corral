import { getTimelinePage, isTimelineEvent, type HostedSession, type TimelineEvent, type TimelinePage } from './api';

const MAX_CACHED_SESSIONS = 50;
const PERSISTED_PAGE_SIZE = 10;
const INITIAL_PAGE_SIZE = 25;
const DB_NAME = 'fleet-console-cache-v1';
const DB_STORE = 'timelines';
const LOCAL_STORAGE_KEY = 'fleet-timeline-cache-v1';
const cache = new Map<string, TimelineEvent[]>();
const inflight = new Map<string, Promise<TimelineEvent[]>>();
const hydrateInflight = new Map<string, Promise<PersistedTimeline | undefined>>();
const pageInfo = new Map<string, Pick<TimelinePage, 'hasMoreBefore' | 'nextBeforeSeq'>>();
const storedAt = new Map<string, number>();
const prefetching = new Set<string>();
const prefetchedAt = new Map<string, number>();

export interface PersistedTimeline {
  key: string;
  events: TimelineEvent[];
  pageInfo: Pick<TimelinePage, 'hasMoreBefore' | 'nextBeforeSeq'>;
  storedAt: number;
  lastAccessed: number;
}

let dbPromise: Promise<IDBDatabase> | undefined;

function openDb(): Promise<IDBDatabase> {
  if (typeof indexedDB === 'undefined') return Promise.reject(new Error('IndexedDB unavailable'));
  dbPromise ??= new Promise((resolve, reject) => {
    const request = indexedDB.open(DB_NAME, 1);
    request.onupgradeneeded = () => request.result.createObjectStore(DB_STORE, { keyPath: 'key' });
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('IndexedDB open failed'));
  });
  return dbPromise;
}

function requestResult<T>(request: IDBRequest<T>): Promise<T> {
  return new Promise((resolve, reject) => {
    request.onsuccess = () => resolve(request.result);
    request.onerror = () => reject(request.error ?? new Error('IndexedDB request failed'));
  });
}

function validPersisted(value: unknown): value is PersistedTimeline {
  if (!value || typeof value !== 'object') return false;
  const record = value as Partial<PersistedTimeline>;
  return typeof record.key === 'string'
    && Array.isArray(record.events)
    && record.events.every(isTimelineEvent)
    && Boolean(record.pageInfo)
    && typeof record.pageInfo?.hasMoreBefore === 'boolean'
    && (record.pageInfo.nextBeforeSeq === null || Number.isInteger(record.pageInfo.nextBeforeSeq))
    && typeof record.storedAt === 'number'
    && typeof record.lastAccessed === 'number';
}

function readLocal(cacheKey: string): PersistedTimeline | undefined {
  try {
    const stored = JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEY) ?? 'null') as { version?: number; records?: unknown[] } | null;
    return stored?.version === 1 ? stored.records?.find((record) => validPersisted(record) && record.key === cacheKey) as PersistedTimeline | undefined : undefined;
  } catch {
    localStorage.removeItem(LOCAL_STORAGE_KEY);
    return undefined;
  }
}

function writeLocal(record: PersistedTimeline): void {
  try {
    const stored = JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEY) ?? 'null') as { version?: number; records?: unknown[] } | null;
    const records = (stored?.version === 1 && Array.isArray(stored.records) ? stored.records.filter(validPersisted) : [])
      .filter((item) => item.key !== record.key);
    records.push(record);
    records.sort((left, right) => right.lastAccessed - left.lastAccessed);
    localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify({ version: 1, records: records.slice(0, MAX_CACHED_SESSIONS) }));
  } catch {}
}

async function readPersistent(cacheKey: string): Promise<PersistedTimeline | undefined> {
  try {
    const db = await openDb();
    const value = await requestResult(db.transaction(DB_STORE).objectStore(DB_STORE).get(cacheKey));
    return validPersisted(value) ? value : undefined;
  } catch {
    return readLocal(cacheKey);
  }
}

async function writePersistent(record: PersistedTimeline): Promise<void> {
  try {
    const db = await openDb();
    await new Promise<void>((resolve, reject) => {
      const transaction = db.transaction(DB_STORE, 'readwrite');
      const store = transaction.objectStore(DB_STORE);
      store.put(record);
      const request = store.getAll();
      request.onsuccess = () => request.result
        .filter(validPersisted)
        .sort((left, right) => right.lastAccessed - left.lastAccessed)
        .slice(MAX_CACHED_SESSIONS)
        .forEach((item) => store.delete(item.key));
      transaction.oncomplete = () => resolve();
      transaction.onerror = () => reject(transaction.error ?? new Error('IndexedDB write failed'));
    });
  } catch {
    writeLocal(record);
  }
}

function key(session: HostedSession): string {
  return [
    session.sourceHost.id,
    session.id,
    session.v2?.recordId ?? '',
    session.v2?.attachmentRevision ?? '',
  ].join('\u001f');
}

function merge(current: TimelineEvent[], next: TimelineEvent[]): TimelineEvent[] {
  const bySeq = new Map(current.map((event) => [event.seq, event]));
  next.forEach((event) => bySeq.set(event.seq, event));
  return [...bySeq.values()].sort((left, right) => left.seq - right.seq);
}

function remember(session: HostedSession, events: TimelineEvent[]): TimelineEvent[] {
  const cacheKey = key(session);
  const value = merge(cache.get(cacheKey) ?? [], events);
  const now = Date.now();
  cache.delete(cacheKey);
  cache.set(cacheKey, value);
  storedAt.set(cacheKey, now);
  while (cache.size > MAX_CACHED_SESSIONS) {
    const oldest = cache.keys().next().value!;
    cache.delete(oldest);
    pageInfo.delete(oldest);
    storedAt.delete(oldest);
  }
  const tail = value.slice(-PERSISTED_PAGE_SIZE);
  const info = pageInfo.get(cacheKey) ?? { hasMoreBefore: value.length >= PERSISTED_PAGE_SIZE, nextBeforeSeq: value[0]?.seq ?? null };
  const persistedInfo = { hasMoreBefore: info.hasMoreBefore || value.length > tail.length, nextBeforeSeq: tail[0]?.seq ?? null };
  void writePersistent({ key: cacheKey, events: tail, pageInfo: persistedInfo, storedAt: now, lastAccessed: now });
  return value;
}

export function hydrateTimelineCache(session: HostedSession): Promise<PersistedTimeline | undefined> {
  const cacheKey = key(session);
  const memory = cache.get(cacheKey);
  if (memory) return Promise.resolve({
    key: cacheKey,
    events: memory,
    pageInfo: pageInfo.get(cacheKey) ?? { hasMoreBefore: false, nextBeforeSeq: memory[0]?.seq ?? null },
    storedAt: storedAt.get(cacheKey) ?? Date.now(),
    lastAccessed: Date.now(),
  });
  const current = hydrateInflight.get(cacheKey);
  if (current) return current;
  const request = readPersistent(cacheKey).then((record) => {
    if (!record) return undefined;
    cache.set(cacheKey, record.events);
    pageInfo.set(cacheKey, record.pageInfo);
    storedAt.set(cacheKey, record.storedAt);
    void writePersistent({ ...record, lastAccessed: Date.now() });
    return record;
  }).finally(() => hydrateInflight.delete(cacheKey));
  hydrateInflight.set(cacheKey, request);
  return request;
}

export function cachedTimeline(session: HostedSession): TimelineEvent[] | undefined {
  const cacheKey = key(session);
  const value = cache.get(cacheKey);
  if (!value) return undefined;
  cache.delete(cacheKey);
  cache.set(cacheKey, value);
  return value;
}

export function rememberTimelineEvents(session: HostedSession, events: TimelineEvent[]): TimelineEvent[] {
  return remember(session, events);
}

export function cachedTimelinePageInfo(session: HostedSession): Pick<TimelinePage, 'hasMoreBefore' | 'nextBeforeSeq'> | undefined {
  return pageInfo.get(key(session));
}

export function loadTimelinePage(session: HostedSession): Promise<TimelineEvent[]> {
  const cached = cachedTimeline(session);
  if (cached) return Promise.resolve(cached);
  const cacheKey = key(session);
  const current = inflight.get(cacheKey);
  if (current) return current;
  const request = hydrateTimelineCache(session)
    .then((persisted) => persisted?.events ?? getTimelinePage(session, { limit: INITIAL_PAGE_SIZE }).then((page) => {
        pageInfo.set(cacheKey, { hasMoreBefore: page.hasMoreBefore, nextBeforeSeq: page.nextBeforeSeq });
        return remember(session, page.events);
      }))
    .finally(() => inflight.delete(cacheKey));
  inflight.set(cacheKey, request);
  return request;
}

export async function loadTimelineAfter(session: HostedSession, afterSeq: number): Promise<TimelineEvent[]> {
  const inflightKey = `${key(session)}\u001fafter:${afterSeq}`;
  const current = inflight.get(inflightKey);
  if (current) return current;
  const request = getTimelinePage(session, { afterSeq, limit: 200 })
    .then((page) => remember(session, page.events))
    .finally(() => inflight.delete(inflightKey));
  inflight.set(inflightKey, request);
  return request;
}

export async function loadTimelineBefore(session: HostedSession, beforeSeq: number): Promise<TimelinePage> {
  const page = await getTimelinePage(session, { beforeSeq, limit: 25 });
  pageInfo.set(key(session), { hasMoreBefore: page.hasMoreBefore, nextBeforeSeq: page.nextBeforeSeq });
  return { ...page, events: remember(session, page.events) };
}

export function prefetchTimelines(sessions: HostedSession[], concurrency = 3): void {
  const queue = sessions.filter((session) => {
    const cacheKey = key(session);
    return !prefetching.has(cacheKey) && Date.now() - (prefetchedAt.get(cacheKey) ?? 0) >= 60_000;
  });
  if (queue.length === 0) return;
  const run = async () => {
    let index = 0;
    const worker = async () => {
      while (index < queue.length) {
        const session = queue[index++];
        const cacheKey = key(session);
        prefetching.add(cacheKey);
        try {
          const persisted = await hydrateTimelineCache(session);
          if (persisted?.events.length) await loadTimelineAfter(session, persisted.events.at(-1)!.seq);
          else await loadTimelinePage(session);
          prefetchedAt.set(cacheKey, Date.now());
        } catch {} finally {
          prefetching.delete(cacheKey);
        }
      }
    };
    await Promise.all(Array.from({ length: Math.min(concurrency, queue.length) }, worker));
  };
  const idle = (window as Window & { requestIdleCallback?: Window['requestIdleCallback'] }).requestIdleCallback;
  if (idle) idle(() => { void run(); }, { timeout: 2_000 });
  else window.setTimeout(() => { void run(); }, 50);
}

function removeLocalMatching(prefix: string): void {
  try {
    const stored = JSON.parse(localStorage.getItem(LOCAL_STORAGE_KEY) ?? 'null') as { version?: number; records?: unknown[] } | null;
    if (stored?.version !== 1 || !Array.isArray(stored.records)) return;
    const records = stored.records.filter((record) => validPersisted(record) && !record.key.startsWith(prefix));
    localStorage.setItem(LOCAL_STORAGE_KEY, JSON.stringify({ version: 1, records }));
  } catch {
    localStorage.removeItem(LOCAL_STORAGE_KEY);
  }
}

export function invalidateTimelineEntry(hostId: string, entryId: string): void {
  const prefix = `${hostId}\u001f${entryId}\u001f`;
  [...cache.keys()].filter((cacheKey) => cacheKey.startsWith(prefix)).forEach((cacheKey) => {
    cache.delete(cacheKey);
    pageInfo.delete(cacheKey);
    storedAt.delete(cacheKey);
  });
  removeLocalMatching(prefix);
  void openDb().then(async (db) => {
    const keys = await requestResult(db.transaction(DB_STORE).objectStore(DB_STORE).getAllKeys());
    const matches = keys.filter((cacheKey): cacheKey is string => typeof cacheKey === 'string' && cacheKey.startsWith(prefix));
    if (matches.length === 0) return;
    const transaction = db.transaction(DB_STORE, 'readwrite');
    matches.forEach((cacheKey) => transaction.objectStore(DB_STORE).delete(cacheKey));
  }).catch(() => {});
}

export function isRetryableTimelineError(error: unknown): boolean {
  return typeof error === 'object' && error !== null && 'retryable' in error && error.retryable === true;
}
