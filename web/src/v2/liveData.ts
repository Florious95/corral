import { useSyncExternalStore } from 'react';
import { getNetwork, isMockMode, isTimelineEvent, readJson, type TimelineEvent, type TimelinePage } from '../api';
import { terminalScreenFixture } from '../../mock/terminal-screen';
import type { HostedV2Entry, HostedV2History } from './mockModel';
import type {
  V2Attachment,
  V2ChooseAccepted,
  V2DeliveryStatus,
  V2HistoryRecord,
  V2Host,
  V2EntryScreen,
  V2KeyAccepted,
  V2KillAccepted,
  V2Snapshot,
  V2TerminalEntry,
  V2TransportState,
  V2UploadAccepted,
  V2TerminalKey,
  V2WriteAccepted,
} from './types';

const CACHE_KEY = 'fleet-v2-snapshot-cache-v2';
const CACHE_VERSION = 3;
const HOST_FAILURE_THRESHOLD = 3;
const STREAM_RETRY_MS = 5_000;
const UNREACHABLE_RETRY_MS = Math.min(STREAM_RETRY_MS * 5, 60_000);

interface HostTarget {
  host: V2Host;
  gatewayUrl: string;
}

export interface V2LiveState {
  snapshots: V2Snapshot[];
  entries: HostedV2Entry[];
  history: HostedV2History[];
  knownHosts: V2Host[];
  loadingHosts: V2Host[];
  failedHosts: V2Host[];
  ready: boolean;
  error: string | null;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function isAttachment(value: unknown): value is V2Attachment {
  return isRecord(value)
    && typeof value.recordId === 'string'
    && typeof value.sessionId === 'string'
    && typeof value.title === 'string'
    && (value.status === 'attached' || value.status === 'suspect')
    && Number.isInteger(value.attachmentRevision)
    && (value.suspectReason === undefined || value.suspectReason === 'delivery_unattributed' || value.suspectReason === 'evidence_conflict' || value.suspectReason === 'record_replaced');
}

function isEntry(value: unknown): value is V2TerminalEntry {
  return isRecord(value)
    && /^e1_[A-Za-z0-9_-]{22}$/.test(String(value.entryId))
    && (value.kind === 'claude' || value.kind === 'codex')
    && typeof value.cwd === 'string'
    && (value.state === 'running' || value.state === 'waiting_input' || value.state === 'idle' || value.state === 'unknown')
    && value.canSend === true
    && typeof value.model === 'string'
    && typeof value.lastActivityAt === 'string'
    && typeof value.lastMessagePreview === 'string'
    && Number.isInteger(value.attachmentRevision)
    && isRecord(value.pane)
    && typeof value.pane.paneId === 'string'
    && typeof value.pane.windowName === 'string'
    && (value.attachment === null || isAttachment(value.attachment))
    && (value.attachment === null || value.attachmentRevision === value.attachment.attachmentRevision);
}

function isHistory(value: unknown): value is V2HistoryRecord {
  return isRecord(value)
    && typeof value.recordId === 'string'
    && typeof value.sessionId === 'string'
    && (value.kind === 'claude' || value.kind === 'codex')
    && typeof value.cwd === 'string'
    && typeof value.title === 'string'
    && typeof value.model === 'string'
    && typeof value.lastActivityAt === 'string'
    && typeof value.preview === 'string';
}

function isSnapshot(value: unknown): value is V2Snapshot {
  return isRecord(value)
    && typeof value.revision === 'string'
    && /^[A-Za-z0-9_-]+$/.test(value.revision)
    && isRecord(value.host)
    && typeof value.host.id === 'string'
    && typeof value.host.name === 'string'
    && Array.isArray(value.entries)
    && value.entries.every(isEntry)
    && Array.isArray(value.history)
    && value.history.every(isHistory);
}

function isWriteAccepted(value: unknown): value is V2WriteAccepted {
  return isRecord(value)
    && value.status === 'accepted'
    && typeof value.entryId === 'string'
    && typeof value.clientNonce === 'string'
    && typeof value.deliveryId === 'string';
}

function isDeliveryStatus(value: unknown): value is V2DeliveryStatus {
  return value === 'accepted' || value === 'echoed' || value === 'unattributed';
}

export class V2EntryWriteError extends Error {
  readonly status: number;
  readonly code: string;
  readonly retryable: boolean;

  constructor(status: number, code: string, message: string, retryable: boolean) {
    super(message);
    this.name = 'V2EntryWriteError';
    this.status = status;
    this.code = code;
    this.retryable = retryable;
  }
}

export class V2TimelineError extends Error {
  readonly status: number;
  readonly retryable: boolean;

  constructor(status: number, message: string, retryable: boolean) {
    super(message);
    this.name = 'V2TimelineError';
    this.status = status;
    this.retryable = retryable;
  }
}

function readCache(): V2Snapshot[] {
  try {
    const stored = JSON.parse(localStorage.getItem(CACHE_KEY) ?? 'null') as { version?: number; snapshots?: unknown } | null;
    if (stored?.version !== CACHE_VERSION || !Array.isArray(stored.snapshots) || !stored.snapshots.every(isSnapshot)) {
      localStorage.removeItem(CACHE_KEY);
      return [];
    }
    return [...new Map(stored.snapshots.map((snapshot) => [snapshot.host.id, snapshot])).values()];
  } catch {
    localStorage.removeItem(CACHE_KEY);
    return [];
  }
}

function writeCache(snapshots: V2Snapshot[]): void {
  try { localStorage.setItem(CACHE_KEY, JSON.stringify({ version: CACHE_VERSION, snapshots })); } catch {}
}

const transport = new Map<string, V2TransportState>();
const failures = new Map<string, number>();
const targets = new Map<string, HostTarget>();
const streams = new Map<string, EventSource>();
const streamTimers = new Map<string, number>();
const streamRefreshTimers = new Map<string, number>();
const inflight = new Map<string, Promise<V2Snapshot | null>>();
const listeners = new Set<() => void>();
let running = false;
let runToken = 0;
let stopTimer: number | undefined;
let snapshots = readCache();
snapshots.forEach((snapshot) => transport.set(snapshot.host.id, 'unknown'));
let loadingHostIds = new Set<string>();
let knownHosts: V2Host[] = snapshots.map((snapshot) => snapshot.host);
let error: string | null = null;

function buildState(ready = true): V2LiveState {
  const entries = snapshots.flatMap((snapshot) => snapshot.entries.map((entry) => ({
    host: snapshot.host,
    entry,
    transportState: transport.get(snapshot.host.id) === 'unreachable' ? 'unreachable' as const : 'reachable' as const,
  })));
  const attached = new Set(entries.flatMap(({ host, entry }) => entry.attachment ? [`${host.id}\u001f${entry.attachment.recordId}`] : []));
  const historyById = new Map<string, HostedV2History>();
  snapshots.forEach((snapshot) => snapshot.history.forEach((record) => {
    const key = `${snapshot.host.id}\u001f${record.recordId}`;
    if (!attached.has(key) && !historyById.has(key)) historyById.set(key, { host: snapshot.host, record });
  }));
  return {
    snapshots,
    entries,
    history: [...historyById.values()],
    knownHosts,
    loadingHosts: knownHosts.filter((host) => loadingHostIds.has(host.id)),
    failedHosts: knownHosts.filter((host) => transport.get(host.id) === 'unreachable'),
    ready,
    error,
  };
}

let state = buildState(false);

function emit(ready = true): void {
  state = buildState(ready);
  listeners.forEach((listener) => listener());
}

function gatewayUrlForIp(ip: string): string {
  return `http://${ip.includes(':') ? `[${ip}]` : ip}:8787`;
}

async function discoverTargets(): Promise<HostTarget[]> {
  const network = await getNetwork();
  const cachedSelf = snapshots.find((snapshot) => snapshot.host.isSelf === true);
  const selfId = network?.self.tailnetIPs?.[0] || network?.self.ip || cachedSelf?.host.id || location.hostname;
  const discovered: HostTarget[] = [{
    host: { id: selfId, name: network?.self.hostname || location.hostname, isSelf: true },
    gatewayUrl: `${location.protocol}//${location.host}`,
  }];
  (network?.peers ?? [])
    .filter((peer) => peer.hasGateway === true && Boolean(peer.tailnetIPs?.[0]))
    .forEach((peer) => {
      const id = peer.tailnetIPs![0];
      discovered.push({ host: { id, name: peer.hostname || id, isSelf: false }, gatewayUrl: gatewayUrlForIp(id) });
    });
  return discovered.sort((left, right) => Number(right.host.isSelf) - Number(left.host.isSelf) || left.host.name.localeCompare(right.host.name));
}

function setTransport(hostId: string, next: V2TransportState): void {
  if (transport.get(hostId) === next) return;
  transport.set(hostId, next);
  if (next === 'reachable') failures.delete(hostId);
  emit();
}

function markFailure(hostId: string): void {
  const count = (failures.get(hostId) ?? 0) + 1;
  failures.set(hostId, count);
  transport.set(hostId, count >= HOST_FAILURE_THRESHOLD ? 'unreachable' : 'unknown');
  emit();
}

function clearHostStream(hostId: string): void {
  streams.get(hostId)?.close();
  streams.delete(hostId);
  const timer = streamTimers.get(hostId);
  if (timer !== undefined) window.clearTimeout(timer);
  streamTimers.delete(hostId);
  const refreshTimer = streamRefreshTimers.get(hostId);
  if (refreshTimer !== undefined) window.clearTimeout(refreshTimer);
  streamRefreshTimers.delete(hostId);
}

function scheduleHostRefresh(target: HostTarget): void {
  if (document.hidden || streamRefreshTimers.has(target.host.id)) return;
  streamRefreshTimers.set(target.host.id, window.setTimeout(() => {
    streamRefreshTimers.delete(target.host.id);
    if (!document.hidden) void refreshHost(target);
  }, 150));
}

function openHostStream(target: HostTarget, revision: string): void {
  if (!running || document.hidden) return;
  clearHostStream(target.host.id);
  const query = revision ? `?afterRevision=${encodeURIComponent(revision)}` : '';
  const source = new EventSource(`${target.gatewayUrl}/api/v2/stream${query}`);
  streams.set(target.host.id, source);
  source.onopen = () => {
    if (!running) return;
    setTransport(target.host.id, 'reachable');
    if (!snapshots.some((snapshot) => snapshot.host.id === target.host.id)) void refreshHost(target);
  };
  const refresh = (event: Event) => {
    try {
      const data = JSON.parse((event as MessageEvent<string>).data) as unknown;
      if (!isRecord(data) || typeof data.previousRevision !== 'string' || typeof data.revision !== 'string') return;
      scheduleHostRefresh(target);
    } catch {}
  };
  source.addEventListener('snapshot_changed', refresh);
  source.addEventListener('snapshot_required', refresh);
  source.onerror = () => {
    source.close();
    streams.delete(target.host.id);
    if (!running) return;
    const delay = transport.get(target.host.id) === 'unreachable' ? UNREACHABLE_RETRY_MS : STREAM_RETRY_MS;
    streamTimers.set(target.host.id, window.setTimeout(() => {
      streamTimers.delete(target.host.id);
      void refreshHost(target);
    }, delay));
  };
}

async function requestSnapshot(target: HostTarget): Promise<V2Snapshot> {
  const response = await fetch(`${target.gatewayUrl}/api/v2/snapshot`, { signal: AbortSignal.timeout(15_000) });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const data = await readJson(response, 'GET v2 snapshot');
  if (!isSnapshot(data)) throw new Error('操作失败：GET v2 snapshot 响应结构错误');
  return { ...data, host: { ...data.host, isSelf: target.host.isSelf } };
}

function refreshHost(target: HostTarget): Promise<V2Snapshot | null> {
  const targetId = target.host.id;
  const current = inflight.get(targetId);
  if (current) return current;
  const firstLoad = !snapshots.some((snapshot) => snapshot.host.id === targetId);
  if (firstLoad) {
    loadingHostIds = new Set(loadingHostIds).add(targetId);
    emit(false);
  }
  const request = requestSnapshot(target)
    .then((snapshot) => {
      const previousSnapshot = snapshots.find((candidate) => candidate.host.id === targetId || candidate.host.id === snapshot.host.id);
      if (previousSnapshot) {
        const currentById = new Map(snapshot.entries.map((entry) => [entry.entryId, entry]));
        const invalidated = previousSnapshot.entries.filter((entry) => {
          const current = currentById.get(entry.entryId);
          return !current || current.attachmentRevision !== entry.attachmentRevision;
        });
        if (invalidated.length > 0) {
          void import('../timelineCache').then(({ invalidateTimelineEntry }) => invalidated.forEach((entry) => invalidateTimelineEntry(snapshot.host.id, entry.entryId)));
        }
      }
      const resolvedTarget = { host: snapshot.host, gatewayUrl: target.gatewayUrl };
      targets.delete(targetId);
      targets.set(snapshot.host.id, resolvedTarget);
      knownHosts = [...knownHosts.filter((host) => host.id !== targetId && host.id !== snapshot.host.id), snapshot.host]
        .sort((left, right) => Number(right.isSelf) - Number(left.isSelf) || left.name.localeCompare(right.name));
      snapshots = [...snapshots.filter((candidate) => candidate.host.id !== targetId && candidate.host.id !== snapshot.host.id), snapshot]
        .sort((left, right) => Number(right.host.isSelf) - Number(left.host.isSelf) || left.host.name.localeCompare(right.host.name));
      writeCache(snapshots);
      error = null;
      if (targetId !== snapshot.host.id) {
        transport.delete(targetId);
        failures.delete(targetId);
      }
      setTransport(snapshot.host.id, 'reachable');
      openHostStream(resolvedTarget, snapshot.revision);
      return snapshot;
    })
    .catch((nextError) => {
      markFailure(targetId);
      if (snapshots.length === 0) error = nextError instanceof Error ? nextError.message : String(nextError);
      const cached = snapshots.find((snapshot) => snapshot.host.id === targetId);
      openHostStream(target, cached?.revision ?? '');
      return null;
    })
    .finally(() => {
      loadingHostIds = new Set([...loadingHostIds].filter((id) => id !== targetId));
      inflight.delete(targetId);
      emit();
    });
  inflight.set(targetId, request);
  return request;
}

async function start(): Promise<void> {
  if (running) return;
  running = true;
  const token = ++runToken;
  const discovered = await discoverTargets();
  if (!running || token !== runToken) return;
  const discoveredHostIds = new Set(discovered.map((target) => target.host.id));
  const retainedSnapshots = snapshots.filter((snapshot) => discoveredHostIds.has(snapshot.host.id));
  if (retainedSnapshots.length !== snapshots.length) {
    snapshots = retainedSnapshots;
    writeCache(snapshots);
  }
  targets.clear();
  discovered.forEach((target) => targets.set(target.host.id, target));
  knownHosts = discovered.map((target) => target.host);
  loadingHostIds = new Set(discovered.map((target) => target.host.id));
  emit(false);
  await Promise.all(discovered.map((target) => refreshHost(target)));
}

function stop(): void {
  stopTimer = undefined;
  running = false;
  runToken += 1;
  [...streams.keys()].forEach(clearHostStream);
}

document.addEventListener('visibilitychange', () => {
  if (!running) return;
  if (document.hidden) {
    [...streams.keys()].forEach(clearHostStream);
    return;
  }
  targets.forEach((target) => { void refreshHost(target); });
});

function subscribe(listener: () => void): () => void {
  if (stopTimer !== undefined) {
    window.clearTimeout(stopTimer);
    stopTimer = undefined;
  }
  listeners.add(listener);
  if (listeners.size === 1) void start();
  return () => {
    listeners.delete(listener);
    if (listeners.size === 0) stopTimer = window.setTimeout(() => {
      if (listeners.size === 0) stop();
    });
  };
}

export function useV2LiveState(): V2LiveState {
  return useSyncExternalStore(subscribe, () => state, () => state);
}

export function subscribeV2LiveState(listener: () => void): () => void {
  return subscribe(listener);
}

export function getV2LiveState(): V2LiveState {
  return state;
}

export async function refreshAllV2Hosts(): Promise<void> {
  if (!running) await start();
  await Promise.all([...targets.values()].map((target) => refreshHost(target)));
}

export function patchV2EntryState(
  hostId: string,
  entryId: string,
  patch: { state?: 'running' | 'waiting_input' | 'idle' | 'gone'; lastActivityAt?: string; lastMessagePreview?: string },
): void {
  let changed = false;
  snapshots = snapshots.map((snapshot) => {
    if (snapshot.host.id !== hostId) return snapshot;
    const entries = snapshot.entries.map((entry) => {
      if (entry.entryId !== entryId) return entry;
      const state = patch.state && patch.state !== 'gone' ? patch.state : entry.state;
      const next = {
        ...entry,
        state,
        lastActivityAt: patch.lastActivityAt ?? entry.lastActivityAt,
        lastMessagePreview: patch.lastMessagePreview ?? entry.lastMessagePreview,
      };
      changed = changed || next.state !== entry.state
        || next.lastActivityAt !== entry.lastActivityAt
        || next.lastMessagePreview !== entry.lastMessagePreview;
      return next;
    });
    return entries === snapshot.entries ? snapshot : { ...snapshot, entries };
  });
  if (!changed) return;
  writeCache(snapshots);
  emit();
}

export function removeV2Entry(hostId: string, entryId: string): void {
  let changed = false;
  snapshots = snapshots.map((snapshot) => {
    if (snapshot.host.id !== hostId) return snapshot;
    const entries = snapshot.entries.filter((entry) => entry.entryId !== entryId);
    if (entries.length === snapshot.entries.length) return snapshot;
    changed = true;
    return { ...snapshot, entries };
  });
  if (!changed) return;
  writeCache(snapshots);
  emit();
}

function baseForHost(hostId: string): string {
  const target = targets.get(hostId);
  if (target) return target.gatewayUrl;
  if (snapshots.some((snapshot) => snapshot.host.id === hostId && snapshot.host.isSelf === true)) return location.origin;
  return hostId === location.hostname ? `${location.protocol}//${location.host}` : gatewayUrlForIp(hostId);
}

async function requestTimeline(url: string, operation: string, signal?: AbortSignal): Promise<unknown> {
  const timeout = AbortSignal.timeout(15_000);
  const response = await fetch(url, { signal: signal ? AbortSignal.any([signal, timeout]) : timeout });
  if (!response.ok) throw new V2TimelineError(response.status, `${operation}: HTTP ${response.status}`, response.status === 410 || response.status === 503);
  return readJson(response, operation);
}

async function readWriteError(response: Response, operation: string): Promise<V2EntryWriteError> {
  try {
    const data = await readJson(response, operation);
    if (isRecord(data) && isRecord(data.error)
      && typeof data.error.code === 'string'
      && typeof data.error.message === 'string'
      && typeof data.error.retryable === 'boolean') {
      return new V2EntryWriteError(response.status, data.error.code, data.error.message, data.error.retryable);
    }
    return new V2EntryWriteError(response.status, 'invalid_error_envelope', `${operation}: HTTP ${response.status}，错误响应结构无效`, false);
  } catch (nextError) {
    return new V2EntryWriteError(response.status, 'invalid_error_envelope', nextError instanceof Error ? nextError.message : String(nextError), false);
  }
}

async function postJson(hostId: string, entryId: string, operation: string, body: Record<string, unknown>): Promise<unknown> {
  const response = await fetch(`${baseForHost(hostId)}/api/v2/entries/${encodeURIComponent(entryId)}/${operation}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (response.status !== 200) throw await readWriteError(response, `POST v2 ${operation}`);
  return readJson(response, `POST v2 ${operation}`);
}

function requireAccepted(data: unknown, entryId: string, clientNonce: string, operation: string): V2WriteAccepted {
  if (!isWriteAccepted(data) || data.entryId !== entryId || data.clientNonce !== clientNonce) {
    throw new Error(`操作失败：POST v2 ${operation} 响应结构错误`);
  }
  return data;
}

export async function sendV2EntryMessage(hostId: string, entryId: string, text: string, clientNonce: string): Promise<V2WriteAccepted> {
  const data = await postJson(hostId, entryId, 'send', { clientNonce, text });
  return requireAccepted(data, entryId, clientNonce, 'send');
}

export async function chooseV2EntryOption(hostId: string, entryId: string, option: number, clientNonce: string): Promise<V2ChooseAccepted> {
  const data = await postJson(hostId, entryId, 'choose', { clientNonce, option });
  const accepted = requireAccepted(data, entryId, clientNonce, 'choose');
  if (!isRecord(data) || data.option !== option) throw new Error('操作失败：POST v2 choose 响应结构错误');
  return { ...accepted, option };
}

export async function killV2Entry(hostId: string, entryId: string, clientNonce: string): Promise<V2KillAccepted> {
  const data = await postJson(hostId, entryId, 'kill', { clientNonce });
  const accepted = requireAccepted(data, entryId, clientNonce, 'kill');
  if (!isRecord(data) || data.killed !== true || !Array.isArray(data.pids) || !data.pids.every((pid) => Number.isInteger(pid))) {
    throw new Error('操作失败：POST v2 kill 响应结构错误');
  }
  return { ...accepted, killed: true, pids: data.pids as number[] };
}

function isEntryScreen(value: unknown): value is V2EntryScreen {
  return isRecord(value)
    && typeof value.entryId === 'string'
    && Number.isInteger(value.cols)
    && Number(value.cols) > 0
    && Number.isInteger(value.rows)
    && Number(value.rows) > 0
    && Number.isInteger(value.cursorX)
    && Number(value.cursorX) >= 0
    && Number(value.cursorX) < Number(value.cols)
    && Number.isInteger(value.cursorY)
    && Number(value.cursorY) >= 0
    && Number(value.cursorY) < Number(value.rows)
    && typeof value.content === 'string'
    && typeof value.hash === 'string'
    && /^[a-f0-9]{64}$/.test(value.hash);
}

export async function getV2EntryScreen(
  hostId: string,
  entryId: string,
  etag?: string,
  signal?: AbortSignal,
): Promise<{ screen: V2EntryScreen | null; etag: string | null }> {
  if (isMockMode()) {
    if (new URLSearchParams(location.search).get('fixture') === 'screen-unavailable') throw new V2TimelineError(404, '服务端版本待升级', false);
    if (signal?.aborted) throw signal.reason;
    return { screen: { ...terminalScreenFixture, entryId }, etag: `"screen-${terminalScreenFixture.hash}"` };
  }
  const headers = new Headers();
  if (etag) headers.set('If-None-Match', etag);
  const response = await fetch(`${baseForHost(hostId)}/api/v2/entries/${encodeURIComponent(entryId)}/screen`, { headers, signal });
  if (response.status === 304) return { screen: null, etag: response.headers.get('ETag') ?? etag ?? null };
  if (!response.ok) throw new V2TimelineError(response.status, `GET v2 entry screen: HTTP ${response.status}`, response.status === 503);
  const data = await readJson(response, 'GET v2 entry screen');
  if (!isEntryScreen(data) || data.entryId !== entryId) throw new Error('操作失败：GET v2 entry screen 响应结构错误');
  return { screen: data, etag: response.headers.get('ETag') };
}

export async function sendV2EntryKey(
  hostId: string,
  entryId: string,
  key: V2TerminalKey,
  clientNonce: string,
): Promise<V2KeyAccepted> {
  if (isMockMode()) return { status: 'accepted', entryId, clientNonce, deliveryId: `mock-${clientNonce}`, key };
  const data = await postJson(hostId, entryId, 'keys', { clientNonce, key });
  const accepted = requireAccepted(data, entryId, clientNonce, 'keys');
  if (!isRecord(data) || data.key !== key) throw new Error('操作失败：POST v2 keys 响应结构错误');
  return { ...accepted, key };
}

export function uploadV2EntryFile(
  hostId: string,
  entryId: string,
  file: File,
  clientNonce: string,
  onProgress: (percent: number) => void,
): Promise<V2UploadAccepted> {
  if (file.size > 20 * 1024 * 1024) return Promise.reject(new Error('附件不能超过 20MB'));
  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open('POST', `${baseForHost(hostId)}/api/v2/entries/${encodeURIComponent(entryId)}/upload`);
    request.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress(Math.round((event.loaded / event.total) * 100));
    };
    request.onerror = () => reject(new Error('附件上传失败'));
    request.onload = () => {
      const contentType = request.getResponseHeader('Content-Type')?.split(';')[0].trim() ?? '';
      let data: unknown;
      if (contentType !== 'application/json' && !contentType.endsWith('+json')) {
        reject(new V2EntryWriteError(request.status, 'invalid_error_envelope', `POST v2 upload：响应格式错误（${contentType || '未知格式'}，不是 JSON）`, false));
        return;
      }
      try { data = JSON.parse(request.responseText) as unknown; } catch (nextError) {
        reject(new V2EntryWriteError(request.status, 'invalid_error_envelope', `POST v2 upload JSON 解析失败（${nextError instanceof Error ? nextError.message : String(nextError)}）`, false));
        return;
      }
      if (request.status !== 200) {
        const error = isRecord(data) && isRecord(data.error) ? data.error : null;
        reject(new V2EntryWriteError(
          request.status,
          error && typeof error.code === 'string' ? error.code : 'invalid_error_envelope',
          error && typeof error.message === 'string' ? error.message : `POST v2 upload: HTTP ${request.status}`,
          Boolean(error?.retryable),
        ));
        return;
      }
      try {
        const accepted = requireAccepted(data, entryId, clientNonce, 'upload');
        if (!isRecord(data) || typeof data.path !== 'string' || !data.path.startsWith('/')) throw new Error('操作失败：POST v2 upload 响应结构错误（缺少绝对路径 path）');
        onProgress(100);
        resolve({ ...accepted, path: data.path });
      } catch (nextError) {
        reject(nextError);
      }
    };
    const body = new FormData();
    body.append('clientNonce', clientNonce);
    body.append('file', file);
    request.send(body);
  });
}

export async function refreshV2Host(hostId: string): Promise<V2Snapshot | null> {
  const target = targets.get(hostId);
  if (!target) throw new Error(`找不到主机 ${hostId}`);
  return refreshHost(target);
}

export async function getV2EntryTimelinePage(
  hostId: string,
  entryId: string,
  options: { afterSeq?: number; beforeSeq?: number; limit?: number } = {},
  signal?: AbortSignal,
): Promise<TimelinePage & { attachment: V2Attachment | null; attachmentRevision: number }> {
  const limit = options.limit ?? 50;
  const query = new URLSearchParams({ limit: String(limit) });
  if (options.afterSeq !== undefined) query.set('afterSeq', String(options.afterSeq));
  if (options.beforeSeq !== undefined) query.set('beforeSeq', String(options.beforeSeq));
  const request = async (currentEntryId: string) => {
    const data = await requestTimeline(`${baseForHost(hostId)}/api/v2/entries/${encodeURIComponent(currentEntryId)}/timeline?${query}`, 'GET v2 entry timeline', signal);
    if (!isRecord(data)
      || !(data.attachment === null || isAttachment(data.attachment))
      || !Number.isInteger(data.attachmentRevision)
      || !Array.isArray(data.events)
      || !data.events.every(isTimelineEvent)) throw new Error('操作失败：GET timeline 响应结构错误');
    const events = [...data.events].sort((left, right) => left.seq - right.seq);
    return {
      attachment: data.attachment,
      attachmentRevision: data.attachmentRevision as number,
      events,
      hasMoreBefore: typeof data.hasMoreBefore === 'boolean' ? data.hasMoreBefore : events.length >= limit,
      nextBeforeSeq: data.nextBeforeSeq === null || Number.isInteger(data.nextBeforeSeq) ? data.nextBeforeSeq as number | null : events[0]?.seq ?? null,
    };
  };
  try {
    return await request(entryId);
  } catch (nextError) {
    if (!(nextError instanceof V2TimelineError) || !nextError.retryable) throw nextError;
    const previous = snapshots.find((snapshot) => snapshot.host.id === hostId)?.entries.find((entry) => entry.entryId === entryId);
    const refreshed = await refreshV2Host(hostId).catch(() => null);
    const rebound = refreshed?.entries.find((entry) => entry.entryId === entryId)
      ?? refreshed?.entries.find((entry) => previous && entry.pane.paneId === previous.pane.paneId)
      ?? refreshed?.entries.find((entry) => previous?.attachment && entry.attachment?.recordId === previous.attachment.recordId);
    if (!rebound) {
      if (nextError.status === 410 && refreshed) throw new V2TimelineError(410, '入口已结束', false);
      throw new V2TimelineError(nextError.status, '重新连接中', true);
    }
    await new Promise((resolve) => window.setTimeout(resolve, 250));
    return request(rebound.entryId);
  }
}

export async function getV2EntryTimeline(hostId: string, entryId: string, afterSeq?: number, signal?: AbortSignal): Promise<{ attachment: V2Attachment | null; attachmentRevision: number; events: TimelineEvent[] }> {
  return getV2EntryTimelinePage(hostId, entryId, { afterSeq }, signal);
}

export async function getV2RecordTimelinePage(
  hostId: string,
  recordId: string,
  options: { afterSeq?: number; beforeSeq?: number; limit?: number } = {},
  signal?: AbortSignal,
): Promise<TimelinePage> {
  const limit = options.limit ?? 50;
  const query = new URLSearchParams({ limit: String(limit) });
  if (options.afterSeq !== undefined) query.set('afterSeq', String(options.afterSeq));
  if (options.beforeSeq !== undefined) query.set('beforeSeq', String(options.beforeSeq));
  const data = await requestTimeline(`${baseForHost(hostId)}/api/v2/records/${encodeURIComponent(recordId)}/timeline?${query}`, 'GET v2 record timeline', signal);
  if (!isRecord(data) || !Array.isArray(data.events) || !data.events.every(isTimelineEvent)) {
    throw new Error('操作失败：GET timeline 响应结构错误');
  }
  const events = [...data.events].sort((left, right) => left.seq - right.seq);
  return {
    events,
    hasMoreBefore: typeof data.hasMoreBefore === 'boolean' ? data.hasMoreBefore : events.length >= limit,
    nextBeforeSeq: data.nextBeforeSeq === null || Number.isInteger(data.nextBeforeSeq) ? data.nextBeforeSeq as number | null : events[0]?.seq ?? null,
  };
}

export async function getV2RecordTimeline(hostId: string, recordId: string, afterSeq?: number, signal?: AbortSignal): Promise<TimelineEvent[]> {
  return (await getV2RecordTimelinePage(hostId, recordId, { afterSeq }, signal)).events;
}

export interface V2EntryStreamHandlers {
  onOpen: () => void;
  onTimeline: (event: TimelineEvent) => void;
  onState: (state: V2TerminalEntry['state']) => void;
  onDelivery: (delivery: { entryId: string; deliveryId: string; clientNonce: string; status: V2DeliveryStatus; recordId?: string | null }) => void;
  onAttachmentChanged: (attachmentRevision: number) => void;
  onEntryRemoved: (reason: string) => void;
  onSnapshotRequired: () => void;
  onError: () => void;
}

interface EntryStreamListener {
  afterSeq: number;
  handlers: V2EntryStreamHandlers;
}

interface EntryStreamConnection {
  source: EventSource;
  listeners: Set<EntryStreamListener>;
  timeline: TimelineEvent[];
  deliveries: Array<{ entryId: string; deliveryId: string; clientNonce: string; status: V2DeliveryStatus; recordId?: string | null }>;
  state?: V2TerminalEntry['state'];
  attachmentRevision?: number;
  removedReason?: string;
  snapshotRequired: boolean;
  open: boolean;
  lastUsed: number;
  idleTimer?: number;
}

const MAX_REUSABLE_ENTRY_STREAMS = 3;
const ENTRY_STREAM_IDLE_MS = 30_000;
const entryStreams = new Map<string, EntryStreamConnection>();

function closeEntryStream(key: string, connection: EntryStreamConnection): void {
  if (connection.idleTimer !== undefined) window.clearTimeout(connection.idleTimer);
  connection.source.close();
  if (entryStreams.get(key) === connection) entryStreams.delete(key);
}

function trimEntryStreamPool(): void {
  if (entryStreams.size <= MAX_REUSABLE_ENTRY_STREAMS) return;
  [...entryStreams.entries()]
    .filter(([, connection]) => connection.listeners.size === 0)
    .sort((left, right) => left[1].lastUsed - right[1].lastUsed)
    .slice(0, entryStreams.size - MAX_REUSABLE_ENTRY_STREAMS)
    .forEach(([key, connection]) => closeEntryStream(key, connection));
}

function replayEntryStream(connection: EntryStreamConnection, listener: EntryStreamListener): void {
  queueMicrotask(() => {
    if (!connection.listeners.has(listener)) return;
    if (connection.open) listener.handlers.onOpen();
    connection.timeline.filter((event) => event.seq > listener.afterSeq).forEach(listener.handlers.onTimeline);
    if (connection.state) listener.handlers.onState(connection.state);
    connection.deliveries.forEach(listener.handlers.onDelivery);
    if (connection.attachmentRevision !== undefined) listener.handlers.onAttachmentChanged(connection.attachmentRevision);
    if (connection.removedReason) listener.handlers.onEntryRemoved(connection.removedReason);
    if (connection.snapshotRequired) listener.handlers.onSnapshotRequired();
  });
}

export function subscribeV2EntryStream(
  hostId: string,
  entryId: string,
  afterSeq: number,
  handlers: V2EntryStreamHandlers,
): () => void {
  const key = `${hostId}\u001f${entryId}`;
  const current = entryStreams.get(key);
  if (current) {
    if (current.idleTimer !== undefined) window.clearTimeout(current.idleTimer);
    current.idleTimer = undefined;
    current.lastUsed = Date.now();
    const listener = { afterSeq, handlers };
    current.listeners.add(listener);
    replayEntryStream(current, listener);
    return () => {
      current.listeners.delete(listener);
      current.lastUsed = Date.now();
      if (window.innerWidth > 768) {
        closeEntryStream(key, current);
        return;
      }
      current.idleTimer = window.setTimeout(() => closeEntryStream(key, current), ENTRY_STREAM_IDLE_MS);
      trimEntryStreamPool();
    };
  }
  const query = new URLSearchParams();
  if (afterSeq > 0) query.set('afterSeq', String(afterSeq));
  const suffix = query.size > 0 ? `?${query}` : '';
  const source = new EventSource(`${baseForHost(hostId)}/api/v2/entries/${encodeURIComponent(entryId)}/stream${suffix}`);
  const listener = { afterSeq, handlers };
  const connection: EntryStreamConnection = {
    source,
    listeners: new Set([listener]),
    timeline: [],
    deliveries: [],
    snapshotRequired: false,
    open: false,
    lastUsed: Date.now(),
  };
  entryStreams.set(key, connection);
  trimEntryStreamPool();
  source.onopen = () => {
    connection.open = true;
    if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onOpen());
  };
  source.addEventListener('timeline', (raw) => {
    try {
      const data = JSON.parse((raw as MessageEvent<string>).data) as unknown;
      if (isTimelineEvent(data)) {
        connection.timeline = [...connection.timeline.filter((event) => event.seq !== data.seq), data]
          .sort((left, right) => left.seq - right.seq)
          .slice(-200);
        if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onTimeline(data));
      }
    } catch {}
  });
  source.addEventListener('state', (raw) => {
    try {
      const data = JSON.parse((raw as MessageEvent<string>).data) as unknown;
      if (isRecord(data) && (data.state === 'running' || data.state === 'waiting_input' || data.state === 'idle' || data.state === 'unknown')) {
        connection.state = data.state;
        if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onState(data.state as V2TerminalEntry['state']));
      }
    } catch {}
  });
  source.addEventListener('delivery', (raw) => {
    try {
      const data = JSON.parse((raw as MessageEvent<string>).data) as unknown;
      if (isRecord(data)
        && data.entryId === entryId
        && typeof data.deliveryId === 'string'
        && typeof data.clientNonce === 'string'
        && isDeliveryStatus(data.status)
        && (data.recordId === undefined || data.recordId === null || typeof data.recordId === 'string')) {
        const delivery = {
          entryId,
          deliveryId: data.deliveryId as string,
          clientNonce: data.clientNonce as string,
          status: data.status,
          recordId: data.recordId ?? null,
        };
        connection.deliveries = [...connection.deliveries.filter((item) => item.clientNonce !== delivery.clientNonce || item.deliveryId !== delivery.deliveryId), delivery].slice(-20);
        if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onDelivery(delivery));
      }
    } catch {}
  });
  source.addEventListener('attachment_changed', (raw) => {
    try {
      const data = JSON.parse((raw as MessageEvent<string>).data) as unknown;
      if (isRecord(data) && data.entryId === entryId && Number.isInteger(data.attachmentRevision) && (data.attachment === null || isAttachment(data.attachment))) {
        connection.timeline = [];
        connection.state = undefined;
        connection.attachmentRevision = data.attachmentRevision as number;
        if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onAttachmentChanged(data.attachmentRevision as number));
      }
    } catch {}
  });
  source.addEventListener('entry_removed', (raw) => {
    try {
      const data = JSON.parse((raw as MessageEvent<string>).data) as unknown;
      if (isRecord(data) && data.entryId === entryId && typeof data.reason === 'string') {
        connection.removedReason = data.reason;
        if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onEntryRemoved(data.reason as string));
      }
    } catch {}
  });
  source.addEventListener('snapshot_required', () => {
    connection.snapshotRequired = true;
    if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onSnapshotRequired());
  });
  source.onerror = () => {
    connection.open = false;
    if (!document.hidden) connection.listeners.forEach((item) => item.handlers.onError());
  };
  return () => {
    connection.listeners.delete(listener);
    connection.lastUsed = Date.now();
    if (window.innerWidth > 768) {
      closeEntryStream(key, connection);
      return;
    }
    connection.idleTimer = window.setTimeout(() => closeEntryStream(key, connection), ENTRY_STREAM_IDLE_MS);
    trimEntryStreamPool();
  };
}

document.addEventListener('visibilitychange', () => {
  if (document.hidden) return;
  entryStreams.forEach((connection) => connection.listeners.forEach((listener) => replayEntryStream(connection, listener)));
});
