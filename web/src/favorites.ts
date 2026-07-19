import { useSyncExternalStore } from 'react';
import { isV2AdapterMode, type HostedSession } from './api';
import { migrateV1Favorites, toggleV2Favorite, useV2Favorites, v2RecordIdentity } from './v2/favorites';
import { getV2LiveState } from './v2/liveData';

const STORAGE_KEY = 'fleet-session-favorites-v1';
const SNAPSHOT_KEY = 'fleet-session-favorite-snapshots-v1';
const STORAGE_VERSION = 1;
const V2_SNAPSHOT_KEY = 'fleet-v2-record-favorite-snapshots-v1';
const HOST_COLORS = ['#176b87', '#8b5e34', '#7a5195', '#2f7d5c', '#b05d3b', '#3f6ea8', '#9a4f68', '#697a30', '#7f674c'];

function readFavorites(): ReadonlySet<string> {
  try {
    const stored = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? 'null') as { version?: number; ids?: unknown } | null;
    if (stored?.version !== STORAGE_VERSION || !Array.isArray(stored.ids)) return new Set();
    return new Set(stored.ids.filter((id): id is string => typeof id === 'string'));
  } catch {
    return new Set();
  }
}

let favorites = readFavorites();
function readSnapshots(): Map<string, HostedSession> {
  try {
    const stored = JSON.parse(localStorage.getItem(SNAPSHOT_KEY) ?? 'null') as { version?: number; sessions?: unknown } | null;
    if (stored?.version !== STORAGE_VERSION || !stored.sessions || typeof stored.sessions !== 'object' || Array.isArray(stored.sessions)) return new Map();
    return new Map(Object.entries(stored.sessions).filter((entry): entry is [string, HostedSession] => {
      const session = entry[1] as Partial<HostedSession>;
      return typeof session?.id === 'string' && typeof session?.sessionId === 'string' && typeof session?.sourceHost?.id === 'string';
    }));
  } catch {
    return new Map();
  }
}

let snapshots = readSnapshots();
function readV2Snapshots(): Map<string, HostedSession> {
  try {
    const stored = JSON.parse(localStorage.getItem(V2_SNAPSHOT_KEY) ?? 'null') as { version?: number; sessions?: unknown } | null;
    if (stored?.version !== 1 || !stored.sessions || typeof stored.sessions !== 'object' || Array.isArray(stored.sessions)) return new Map();
    return new Map(Object.entries(stored.sessions).filter((entry): entry is [string, HostedSession] => {
      const session = entry[1] as Partial<HostedSession>;
      return typeof session?.id === 'string' && typeof session?.sourceHost?.id === 'string' && session?.v2?.recordId !== undefined;
    }));
  } catch {
    return new Map();
  }
}

let v2Snapshots = readV2Snapshots();
const listeners = new Set<() => void>();

function emit(): void {
  listeners.forEach((listener) => listener());
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

export function sessionIdentity(session: HostedSession): string {
  if (session.v2?.recordId) return v2RecordIdentity(session.sourceHost.id, session.v2.recordId);
  if (session.v2) return `${session.sourceHost.id}\u001fentry:${session.v2.entryId ?? session.id}`;
  return `${session.sourceHost.id}\u001f${session.kind}\u001f${session.sessionId}`;
}

export function useFavorites(): ReadonlySet<string> {
  const v1 = useSyncExternalStore(subscribe, () => favorites, () => favorites);
  const v2 = useV2Favorites();
  return isV2AdapterMode() ? v2 : v1;
}

export function toggleFavorite(session: HostedSession): void {
  if (session.v2) {
    if (session.v2.recordId) toggleV2Favorite(session.sourceHost.id, session.v2.recordId);
    return;
  }
  const next = new Set(favorites);
  const identity = sessionIdentity(session);
  if (next.has(identity)) {
    next.delete(identity);
    snapshots.delete(identity);
  } else {
    next.add(identity);
    snapshots.set(identity, session);
  }
  favorites = next;
  localStorage.setItem(STORAGE_KEY, JSON.stringify({ version: STORAGE_VERSION, ids: [...favorites] }));
  localStorage.setItem(SNAPSHOT_KEY, JSON.stringify({ version: STORAGE_VERSION, sessions: Object.fromEntries(snapshots) }));
  emit();
}

export function rememberFavoriteSessions(sessions: HostedSession[], ids: ReadonlySet<string>): void {
  if (isV2AdapterMode()) {
    migrateV1Favorites(getV2LiveState().snapshots);
    let changed = false;
    for (const session of sessions) {
      const identity = sessionIdentity(session);
      if (!session.v2?.recordId || !ids.has(identity)) continue;
      v2Snapshots.set(identity, session);
      changed = true;
    }
    if (changed) localStorage.setItem(V2_SNAPSHOT_KEY, JSON.stringify({ version: 1, sessions: Object.fromEntries(v2Snapshots) }));
    return;
  }
  let changed = false;
  for (const session of sessions) {
    const identity = sessionIdentity(session);
    if (!ids.has(identity)) continue;
    snapshots.set(identity, session);
    changed = true;
  }
  if (changed) localStorage.setItem(SNAPSHOT_KEY, JSON.stringify({ version: STORAGE_VERSION, sessions: Object.fromEntries(snapshots) }));
}

export function getFavoriteSnapshot(identity: string): HostedSession | undefined {
  return (isV2AdapterMode() ? v2Snapshots : snapshots).get(identity);
}

export function findFavoriteSnapshot(hostId: string, sessionId: string): HostedSession | undefined {
  return [...(isV2AdapterMode() ? v2Snapshots : snapshots).values()].find((session) => session.sourceHost.id === hostId && session.id === sessionId);
}

export function hostIdentityColor(hostId: string): string {
  let hash = 2166136261;
  for (let index = 0; index < hostId.length; index += 1) {
    hash ^= hostId.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return HOST_COLORS[(hash >>> 0) % HOST_COLORS.length];
}

window.addEventListener('storage', (event) => {
  if (event.key === STORAGE_KEY) favorites = readFavorites();
  else if (event.key === SNAPSHOT_KEY) snapshots = readSnapshots();
  else if (event.key === V2_SNAPSHOT_KEY) v2Snapshots = readV2Snapshots();
  else return;
  emit();
});
