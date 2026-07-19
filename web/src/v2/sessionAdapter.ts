import sessionsFixture from '../../mock/sessions.json';
import { stressSessionsFixture } from '../../mock/sessions-stress';
import type { HostedSession, SessionHost, SessionStateUpdate } from '../api';
import type { SessionsState } from '../hooks/useSessions';
import { getV2LiveState, patchV2EntryState, refreshAllV2Hosts, removeV2Entry, subscribeV2LiveState, type V2LiveState } from './liveData';
import type { V2Snapshot, V2TransportState } from './types';

interface FixtureSession {
  id: string;
  kind: 'claude' | 'codex';
  cwd: string;
  title: string;
  sessionId: string;
  state: 'running' | 'waiting_input' | 'idle' | 'gone';
  canSend: boolean;
  model: string;
  lastActivityAt: string;
  lastMessagePreview: string;
  live: boolean;
}

interface FixtureHost {
  id: string;
  name: string;
  sessions: FixtureSession[];
}

const fixtureCompatIds = new Map<string, string>();

function gatewayUrl(hostId: string, isSelf: boolean): string {
  if (isSelf) return `${location.protocol}//${location.host}`;
  return `http://${hostId.includes(':') ? `[${hostId}]` : hostId}:8787`;
}

function mockEntryId(hostIndex: number, sessionIndex: number): string {
  return `e1_${`${hostIndex}_${sessionIndex}`.padEnd(22, 'A').slice(0, 22)}`;
}

let fixtureCacheKey = '';
let fixtureCache: V2LiveState | null = null;

function fixtureState(): V2LiveState {
  const fixtureKey = new URLSearchParams(location.search).get('fixture') ?? 'sessions';
  if (fixtureCache?.ready && fixtureCacheKey === fixtureKey) return fixtureCache;
  const fixture = fixtureKey === 'stress'
    ? stressSessionsFixture
    : sessionsFixture;
  fixtureCompatIds.clear();
  const hosts = (fixture as { hosts: FixtureHost[] }).hosts;
  const snapshots: V2Snapshot[] = hosts.map((host, hostIndex) => ({
    revision: `rv1_fixture_${hostIndex + 1}`,
    host: { id: host.id, name: host.name, isSelf: hostIndex === 0 },
    entries: host.sessions.filter((session) => session.live).map((session, sessionIndex) => {
      const entryId = mockEntryId(hostIndex, sessionIndex);
      fixtureCompatIds.set(`${host.id}\u001f${entryId}`, session.id);
      return {
        entryId,
        kind: session.kind,
        cwd: session.cwd,
        state: session.state === 'gone' ? 'idle' : session.state,
        canSend: true as const,
        model: session.model,
        lastActivityAt: session.lastActivityAt,
        lastMessagePreview: session.lastMessagePreview,
        attachmentRevision: 1,
        pane: { paneId: `%fixture-${hostIndex}-${sessionIndex}`, windowName: session.title },
        attachment: {
          recordId: session.id,
          sessionId: session.sessionId,
          title: session.title,
          status: 'attached' as const,
          attachmentRevision: 1,
        },
      };
    }),
    history: host.sessions.filter((session) => !session.live).map((session) => ({
      recordId: session.id,
      sessionId: session.sessionId,
      kind: session.kind,
      cwd: session.cwd,
      title: session.title,
      model: session.model,
      lastActivityAt: session.lastActivityAt,
      preview: session.lastMessagePreview,
    })),
  }));
  const unreachable = new Set(hosts.filter((host) => host.sessions.some((session) => session.live && !session.canSend)).map((host) => host.id));
  fixtureCacheKey = fixtureKey;
  fixtureCache = {
    snapshots,
    entries: snapshots.flatMap((snapshot) => snapshot.entries.map((entry) => ({
      host: snapshot.host,
      entry,
      transportState: unreachable.has(snapshot.host.id) ? 'unreachable' as const : 'reachable' as const,
    }))),
    history: snapshots.flatMap((snapshot) => snapshot.history.map((record) => ({ host: snapshot.host, record }))),
    knownHosts: snapshots.map((snapshot) => snapshot.host),
    loadingHosts: [],
    failedHosts: snapshots.filter((snapshot) => unreachable.has(snapshot.host.id)).map((snapshot) => snapshot.host),
    ready: true,
    error: null,
  };
  return fixtureCache;
}

function sourceHost(id: string, name: string, isSelf: boolean): SessionHost {
  return { id, name, isSelf, gatewayUrl: gatewayUrl(id, isSelf) };
}

function projectEntry(host: V2LiveState['entries'][number]['host'], entry: V2LiveState['entries'][number]['entry'], transportState: V2TransportState): HostedSession {
  const compatId = fixtureCompatIds.get(`${host.id}\u001f${entry.entryId}`)
    ?? (new URLSearchParams(location.search).get('compat') === 'v1' ? entry.attachment?.sessionId : undefined);
  const cwdName = entry.cwd.split('/').filter(Boolean).at(-1) || entry.cwd;
  return {
    id: compatId ?? entry.entryId,
    kind: entry.kind,
    cwd: entry.cwd,
    title: entry.attachment?.title ?? `${cwdName} · ${entry.kind === 'claude' ? 'Claude' : 'Codex'}`,
    sessionId: entry.attachment?.sessionId ?? entry.entryId,
    sessionFile: '',
    state: entry.state === 'unknown' ? 'idle' : entry.state,
    canSend: entry.canSend,
    model: entry.model,
    lastActivityAt: entry.lastActivityAt,
    lastMessagePreview: entry.attachment ? entry.lastMessagePreview : '',
    live: true,
    sourceHost: sourceHost(host.id, host.name, host.isSelf === true),
    v2: {
      target: 'entry',
      entryId: entry.entryId,
      paneId: entry.pane.paneId,
      recordId: entry.attachment?.recordId ?? null,
      attachmentRevision: entry.attachmentRevision,
      transportState,
      attachmentStatus: entry.attachment?.status,
    },
  };
}

function projectHistory(host: V2LiveState['history'][number]['host'], record: V2LiveState['history'][number]['record']): HostedSession {
  return {
    id: record.recordId,
    kind: record.kind,
    cwd: record.cwd,
    title: record.title,
    sessionId: record.sessionId,
    sessionFile: '',
    state: 'gone',
    canSend: false,
    model: record.model,
    lastActivityAt: record.lastActivityAt,
    lastMessagePreview: record.preview,
    live: false,
    sourceHost: sourceHost(host.id, host.name, host.isSelf === true),
    v2: { target: 'record', recordId: record.recordId, attachmentRevision: 0, transportState: 'reachable' },
  };
}

let lastSource: V2LiveState | null = null;
let lastProjected: SessionsState | null = null;

function project(source: V2LiveState): SessionsState {
  if (source === lastSource && lastProjected) return lastProjected;
  const sessions = [
    ...source.entries.map(({ host, entry, transportState }) => projectEntry(host, entry, transportState)),
    ...source.history.map(({ host, record }) => projectHistory(host, record)),
  ];
  const snapshotHostIds = new Set(source.snapshots.map((snapshot) => snapshot.host.id));
  lastSource = source;
  lastProjected = {
    sessions,
    err: source.error,
    loading: !source.ready || source.loadingHosts.length > 0,
    lastFetched: source.ready ? Date.now() : 0,
    hostsTotal: source.knownHosts.length,
    hostsSucceeded: snapshotHostIds.size,
    failedHosts: source.failedHosts.map((host) => host.name),
    unreachableHostIds: source.failedHosts.map((host) => host.id),
    loadingHosts: source.loadingHosts.map((host) => sourceHost(host.id, host.name, host.isSelf === true)),
  };
  return lastProjected;
}

function fixtureMode(): boolean {
  return new URLSearchParams(location.search).has('fixture');
}

export function subscribeV2Sessions(listener: () => void): () => void {
  if (fixtureMode()) return () => {};
  return subscribeV2LiveState(listener);
}

export function getV2SessionsSnapshot(): SessionsState {
  return project(fixtureMode() ? fixtureState() : getV2LiveState());
}

export async function refreshV2Sessions(): Promise<void> {
  if (!fixtureMode()) await refreshAllV2Hosts();
}

export function updateV2SessionState(hostId: string, sessionId: string, patch: SessionStateUpdate): void {
  if (fixtureMode()) return;
  const session = getV2SessionsSnapshot().sessions.find((candidate) => candidate.sourceHost.id === hostId && candidate.id === sessionId);
  if (session?.v2?.target === 'entry' && session.v2.entryId) patchV2EntryState(hostId, session.v2.entryId, patch);
}

export function markV2SessionKilled(hostId: string, sessionId: string): void {
  if (fixtureMode()) return;
  const session = getV2SessionsSnapshot().sessions.find((candidate) => candidate.sourceHost.id === hostId && candidate.id === sessionId);
  if (session?.v2?.target === 'entry' && session.v2.entryId) removeV2Entry(hostId, session.v2.entryId);
}
