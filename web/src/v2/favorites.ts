import { useSyncExternalStore } from 'react';
import type { V2Snapshot } from './types';

const STORAGE_KEY = 'fleet-v2-record-favorites-v2';
const STORAGE_VERSION = 2;
const LEGACY_KEY = 'fleet-session-favorites-v1';

interface V2FavoriteRecord {
  type: 'record';
  hostId: string;
  recordId: string;
}

interface FavoriteStore {
  version: 2;
  records: V2FavoriteRecord[];
  migratedLegacyIds: string[];
}

function emptyStore(): FavoriteStore {
  return { version: STORAGE_VERSION, records: [], migratedLegacyIds: [] };
}

export function v2RecordIdentity(hostId: string, recordId: string): string {
  return `${hostId}\u001f${recordId}`;
}

function readStore(): FavoriteStore {
  try {
    const value = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? 'null') as Partial<FavoriteStore> | null;
    if (value?.version !== STORAGE_VERSION || !Array.isArray(value.records) || !Array.isArray(value.migratedLegacyIds)) return emptyStore();
    const records = value.records.filter((record): record is V2FavoriteRecord => record?.type === 'record' && typeof record.hostId === 'string' && typeof record.recordId === 'string');
    return { version: STORAGE_VERSION, records, migratedLegacyIds: value.migratedLegacyIds.filter((id): id is string => typeof id === 'string') };
  } catch {
    return emptyStore();
  }
}

let store = readStore();
let favoriteIds = new Set(store.records.map((record) => v2RecordIdentity(record.hostId, record.recordId)));
const listeners = new Set<() => void>();

function persist(): void {
  try { localStorage.setItem(STORAGE_KEY, JSON.stringify(store)); } catch {}
  favoriteIds = new Set(store.records.map((record) => v2RecordIdentity(record.hostId, record.recordId)));
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function useV2Favorites(): ReadonlySet<string> {
  return useSyncExternalStore(subscribe, () => favoriteIds, () => favoriteIds);
}

export function toggleV2Favorite(hostId: string, recordId: string): void {
  const identity = v2RecordIdentity(hostId, recordId);
  const next = store.records.filter((record) => v2RecordIdentity(record.hostId, record.recordId) !== identity);
  if (next.length === store.records.length) next.push({ type: 'record', hostId, recordId });
  store = { ...store, records: next };
  persist();
}

function legacyIds(): string[] {
  try {
    const value = JSON.parse(localStorage.getItem(LEGACY_KEY) ?? 'null') as { version?: number; ids?: unknown } | null;
    return value?.version === 1 && Array.isArray(value.ids) ? value.ids.filter((id): id is string => typeof id === 'string') : [];
  } catch {
    return [];
  }
}

export function migrateV1Favorites(snapshots: V2Snapshot[]): void {
  const migrated = new Set(store.migratedLegacyIds);
  const records = new Map(store.records.map((record) => [v2RecordIdentity(record.hostId, record.recordId), record]));
  let changed = false;
  for (const identity of legacyIds()) {
    if (migrated.has(identity)) continue;
    const [hostId, kind, sessionId] = identity.split('\u001f');
    const snapshot = snapshots.find((candidate) => candidate.host.id === hostId);
    if (!snapshot) continue;
    const entryRecord = snapshot.entries.find((entry) => entry.kind === kind && entry.attachment?.sessionId === sessionId)?.attachment?.recordId;
    const historyRecord = snapshot.history.find((record) => record.kind === kind && record.sessionId === sessionId)?.recordId;
    const recordId = entryRecord ?? historyRecord;
    if (recordId) records.set(v2RecordIdentity(hostId, recordId), { type: 'record', hostId, recordId });
    migrated.add(identity);
    changed = true;
  }
  if (!changed) return;
  store = { version: STORAGE_VERSION, records: [...records.values()], migratedLegacyIds: [...migrated] };
  persist();
}

window.addEventListener('storage', (event) => {
  if (event.key !== STORAGE_KEY) return;
  store = readStore();
  favoriteIds = new Set(store.records.map((record) => v2RecordIdentity(record.hostId, record.recordId)));
  listeners.forEach((listener) => listener());
});
