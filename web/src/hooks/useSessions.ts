import { useEffect, useSyncExternalStore } from 'react';
import { isV2AdapterMode, listSessions, SESSION_POLL_INTERVAL_MS, type HostedSession, type SessionHost, type SessionStateUpdate } from '../api';
import { getV2SessionsSnapshot, markV2SessionKilled, refreshV2Sessions, subscribeV2Sessions, updateV2SessionState } from '../v2/sessionAdapter';

export interface SessionsState {
  sessions: HostedSession[];
  err: string | null;
  loading: boolean;
  lastFetched: number;
  hostsTotal: number;
  hostsSucceeded: number;
  failedHosts: string[];
  unreachableHostIds: string[];
  loadingHosts: SessionHost[];
}

const CACHE_VERSION = '2';
const CACHE_VERSION_KEY = 'fleet-console-cache-version';
const CACHE_KEY = 'fleet-sessions-cache-v2';
const OLD_KEYS = [
  'fleet-sessions-cache-v1',
  'corral-panes-cache-v1',
  'corral-favorites-v2',
  'corral-favorites',
  'corral-selected-host',
  'corral-hosts-cache',
  'corral-hosts-cache-ver',
];

function readCache(): HostedSession[] {
  try {
    if (localStorage.getItem(CACHE_VERSION_KEY) !== CACHE_VERSION) {
      localStorage.removeItem(CACHE_KEY);
      OLD_KEYS.forEach((key) => localStorage.removeItem(key));
      localStorage.setItem(CACHE_VERSION_KEY, CACHE_VERSION);
      return [];
    }
    const raw = localStorage.getItem(CACHE_KEY);
    return raw ? (JSON.parse(raw) as HostedSession[]) : [];
  } catch {
    return [];
  }
}

function writeCache(sessions: HostedSession[]): void {
  try {
    localStorage.setItem(CACHE_KEY, JSON.stringify(sessions));
  } catch {}
}

const cachedSessions = readCache();
const cachedHostCount = new Set(cachedSessions.map((session) => session.sourceHost.id)).size;

let state: SessionsState = {
  sessions: cachedSessions,
  err: null,
  loading: false,
  lastFetched: 0,
  hostsTotal: cachedHostCount,
  hostsSucceeded: cachedHostCount,
  failedHosts: [],
  unreachableHostIds: [],
  loadingHosts: [],
};

const listeners = new Set<() => void>();
const subscribe = (listener: () => void) => {
  listeners.add(listener);
  return () => listeners.delete(listener);
};
const getSnapshot = () => state;
const emit = () => listeners.forEach((listener) => listener());

let inflight: Promise<void> | null = null;
let pollRound = 0;
const deadConfirmations = new Map<string, { round: number; count: number }>();
const closedSessions = new Set<string>();

function sessionKey(hostId: string, sessionId: string): string {
  return `${hostId}:${sessionId}`;
}

function stabilizeLive(sessions: HostedSession[], previous: HostedSession[], round: number): HostedSession[] {
  const previousByKey = new Map(previous.map((session) => [`${session.sourceHost.id}:${session.id}`, session]));
  return sessions.map((session) => {
    const key = sessionKey(session.sourceHost.id, session.id);
    if (session.live) {
      deadConfirmations.delete(key);
      return session;
    }
    const prior = previousByKey.get(key);
    if (!prior?.live) return session;
    const confirmation = deadConfirmations.get(key);
    const count = confirmation?.round === round ? confirmation.count : (confirmation?.count ?? 0) + 1;
    deadConfirmations.set(key, { round, count });
    return count >= 2 ? session : { ...session, live: true, state: prior.state, canSend: prior.canSend };
  });
}

async function fetchOnce(): Promise<void> {
  if (inflight) return inflight;
  const round = ++pollRound;
  state = { ...state, loading: true };
  emit();
  inflight = (async () => {
    try {
      const result = await listSessions((progress) => {
        const stabilizedSessions = stabilizeLive(progress.sessions, state.sessions, round);
        const discoveredHostIds = new Set(progress.knownHosts.map((host) => host.id));
        const succeededHostIds = new Set(progress.succeededHostIds);
        const unreachableHostIds = new Set(progress.unreachableHostIds);
        const sessions = [
          ...state.sessions.filter((session) =>
            discoveredHostIds.has(session.sourceHost.id)
            && !succeededHostIds.has(session.sourceHost.id)),
          ...stabilizedSessions,
        ].map((session) => {
          if (closedSessions.has(sessionKey(session.sourceHost.id, session.id))) return { ...session, live: false, canSend: false, state: 'gone' as const };
          return unreachableHostIds.has(session.sourceHost.id) ? { ...session, canSend: false } : session;
        });
        const renderedHostIds = new Set(sessions.map((session) => session.sourceHost.id));
        const loadingHosts = progress.loadingHosts.filter((host) => !renderedHostIds.has(host.id));
        state = { ...state, ...progress, sessions, loadingHosts, err: null, loading: progress.loadingHosts.length > 0 };
        emit();
      });
      state = { ...state, ...result, sessions: state.sessions, loadingHosts: [], err: null, loading: false, lastFetched: Date.now() };
      writeCache(state.sessions);
    } catch (error) {
      state = {
        ...state,
        err: error instanceof Error ? error.message : String(error),
        loading: false,
        loadingHosts: [],
      };
    } finally {
      inflight = null;
      emit();
    }
  })();
  return inflight;
}

let subscribers = 0;
let timer: number | undefined;
function startPolling() {
  if (timer !== undefined) return;
  timer = window.setInterval(fetchOnce, SESSION_POLL_INTERVAL_MS);
}

export function useSessions(): SessionsState {
  const v2 = isV2AdapterMode();
  const snapshot = useSyncExternalStore(
    v2 ? subscribeV2Sessions : subscribe,
    v2 ? getV2SessionsSnapshot : getSnapshot,
    v2 ? getV2SessionsSnapshot : getSnapshot,
  );
  useEffect(() => {
    if (v2) return;
    subscribers += 1;
    startPolling();
    if (state.lastFetched === 0 && !inflight) fetchOnce();
    return () => {
      subscribers -= 1;
      if (subscribers === 0 && timer !== undefined) {
        window.clearInterval(timer);
        timer = undefined;
      }
    };
  }, [v2]);
  return snapshot;
}

export function refreshSessions(): Promise<void> {
  if (isV2AdapterMode()) return refreshV2Sessions();
  return fetchOnce();
}

export function updateSessionState(hostId: string, sessionId: string, patch: SessionStateUpdate): void {
  if (isV2AdapterMode()) {
    updateV2SessionState(hostId, sessionId, patch);
    return;
  }
  let changed = false;
  const sessions = state.sessions.map((session) => {
    if (session.sourceHost.id !== hostId || session.id !== sessionId) return session;
    changed = true;
    if (closedSessions.has(sessionKey(hostId, sessionId))) return { ...session, live: false, canSend: false, state: 'gone' as const };
    const stablePatch = patch.live === false && session.live
      ? { ...patch, live: true, state: session.state, canSend: session.canSend }
      : patch;
    return { ...session, ...stablePatch };
  });
  if (!changed) return;
  state = { ...state, sessions };
  writeCache(sessions);
  emit();
}

export function markSessionKilled(hostId: string, sessionId: string): void {
  if (isV2AdapterMode()) {
    markV2SessionKilled(hostId, sessionId);
    return;
  }
  closedSessions.add(sessionKey(hostId, sessionId));
  const sessions = state.sessions.map((session) => session.sourceHost.id === hostId && session.id === sessionId
    ? { ...session, live: false, canSend: false, state: 'gone' as const }
    : session);
  state = { ...state, sessions };
  writeCache(sessions);
  emit();
}
