import sessionsFixture from '../mock/sessions.json';
import { stressSessionsFixture } from '../mock/sessions-stress';
import { deepTimelineFixture } from '../mock/timeline-deep';

export type SessionKind = 'claude' | 'codex';
export type SessionState = 'running' | 'waiting_input' | 'idle' | 'gone';

export interface AgentSession {
  id: string;
  kind: SessionKind;
  cwd: string;
  title: string;
  sessionId: string;
  sessionFile: string;
  state: SessionState;
  canSend: boolean;
  model: string;
  lastActivityAt: string;
  lastMessagePreview: string;
  live: boolean;
}

export interface SessionHost {
  id: string;
  name: string;
  gatewayUrl?: string;
  isSelf?: boolean;
}

export interface HostedSession extends AgentSession {
  sourceHost: SessionHost;
  v2?: {
    target: 'entry' | 'record';
    entryId?: string;
    paneId?: string;
    recordId: string | null;
    attachmentRevision: number;
    transportState: 'reachable' | 'unknown' | 'unreachable';
    attachmentStatus?: 'attached' | 'suspect';
  };
}

export interface SessionsLoadResult {
  sessions: HostedSession[];
  hostsTotal: number;
  hostsSucceeded: number;
  failedHosts: string[];
  unreachableHostIds: string[];
}

export interface SessionsProgress extends SessionsLoadResult {
  loadingHosts: SessionHost[];
  knownHosts: SessionHost[];
  succeededHostIds: string[];
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

export async function readJson(response: Response, operation: string): Promise<unknown> {
  const contentType = response.headers.get('content-type')?.split(';')[0].trim() ?? '';
  if (contentType !== 'application/json' && !contentType.endsWith('+json')) {
    throw new Error(`操作失败：${operation} 返回 ${contentType || '未知格式'}，不是 JSON`);
  }
  try {
    return await response.json() as unknown;
  } catch (error) {
    throw new Error(`操作失败：${operation} JSON 解析失败（${error instanceof Error ? error.message : String(error)}）`);
  }
}

function isAgentSession(value: unknown): value is AgentSession {
  if (!isRecord(value)) return false;
  return typeof value.id === 'string'
    && (value.kind === 'claude' || value.kind === 'codex')
    && typeof value.cwd === 'string'
    && typeof value.title === 'string'
    && typeof value.sessionId === 'string'
    && typeof value.sessionFile === 'string'
    && (value.state === 'running' || value.state === 'waiting_input' || value.state === 'idle' || value.state === 'gone')
    && typeof value.canSend === 'boolean'
    && typeof value.model === 'string'
    && typeof value.lastActivityAt === 'string'
    && typeof value.lastMessagePreview === 'string'
    && typeof value.live === 'boolean';
}

interface MockHost {
  id: string;
  name: string;
  sessions: AgentSession[];
}

interface SessionsFixture {
  hosts: MockHost[];
}

export type TimelineEvent =
  | { seq: number; ts: string; type: 'user_message' | 'assistant_message' | 'status'; text: string }
  | { seq: number; ts: string; type: 'skill_load'; skill: string; text: string }
  | { seq: number; ts: string; type: 'tool_use'; tool: string; summary: string; input: string }
  | { seq: number; ts: string; type: 'tool_result'; tool: string; ok: boolean; output: string; truncated?: boolean };

export interface TimelinePage {
  events: TimelineEvent[];
  hasMoreBefore: boolean;
  nextBeforeSeq: number | null;
}

export function isTimelineEvent(value: unknown): value is TimelineEvent {
  if (!isRecord(value) || typeof value.seq !== 'number' || typeof value.ts !== 'string' || typeof value.type !== 'string') return false;
  if (value.type === 'user_message' || value.type === 'assistant_message' || value.type === 'status') return typeof value.text === 'string';
  if (value.type === 'skill_load') return typeof value.skill === 'string' && typeof value.text === 'string';
  if (value.type === 'tool_use') return typeof value.tool === 'string' && typeof value.summary === 'string' && typeof value.input === 'string';
  return value.type === 'tool_result'
    && typeof value.tool === 'string'
    && typeof value.ok === 'boolean'
    && typeof value.output === 'string'
    && (value.truncated === undefined || typeof value.truncated === 'boolean');
}

const timelineFixtures = import.meta.glob('../mock/timeline-*.json', {
  eager: true,
  import: 'default',
}) as Record<string, TimelineEvent[]>;

function httpBase(): string {
  const env = import.meta.env.VITE_GATEWAY;
  if (env) return env;
  return `${location.protocol}//${location.host}`;
}

export function isMockMode(): boolean {
  const queryMode = new URLSearchParams(location.search).get('data');
  if (queryMode === 'real') return false;
  if (queryMode === 'v2') return new URLSearchParams(location.search).has('fixture');
  if (queryMode === 'mock') return true;
  return import.meta.env.VITE_DATA_MODE === 'mock';
}

export function isV2AdapterMode(): boolean {
  const mode = new URLSearchParams(location.search).get('data');
  return mode === null || mode === 'v2';
}

export function sessionCanWrite(session: HostedSession): boolean {
  if (session.v2) {
    return session.v2.target === 'entry'
      && session.live
      && session.canSend
      && session.v2.transportState === 'reachable';
  }
  return session.live && session.canSend;
}

function clientNonce(): string {
  return typeof crypto.randomUUID === 'function'
    ? crypto.randomUUID()
    : `v2-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function v2EntryId(session: HostedSession): string {
  if (session.v2?.target !== 'entry' || !session.v2.entryId) throw new Error('该目标不是可写终端入口');
  return session.v2.entryId;
}

async function rethrowV2WriteError(session: HostedSession, error: unknown): Promise<never> {
  if (isRecord(error) && error.status === 410) {
    const { refreshV2Host } = await import('./v2/liveData');
    await refreshV2Host(session.sourceHost.id).catch(() => null);
    throw new SessionSendError(410, `入口已失效：${typeof error.message === 'string' ? error.message : 'entry generation has exited'}`);
  }
  throw error;
}

function gatewayUrlForIp(ip: string): string {
  return `http://${ip.includes(':') ? `[${ip}]` : ip}:8787`;
}

export const SESSION_POLL_INTERVAL_MS = 5000;
const HOST_FAILURE_THRESHOLD = 3;
const UNREACHABLE_POLL_INTERVAL_MS = Math.min(SESSION_POLL_INTERVAL_MS * 5, 60_000);
const hostReachability = new Map<string, { consecutiveFailures: number; unreachable: boolean; nextAttemptAt: number }>();

async function fetchHostSessions(host: SessionHost): Promise<HostedSession[]> {
  const response = await fetch(`${host.gatewayUrl || httpBase()}/api/sessions`);
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  const data = await readJson(response, 'GET sessions');
  if (!isRecord(data) || !Array.isArray(data.sessions) || !data.sessions.every(isAgentSession)) {
    throw new Error('操作失败：GET sessions 响应结构错误');
  }
  return data.sessions.map((session) => ({ ...session, sourceHost: host }));
}

export async function listSessions(onProgress?: (progress: SessionsProgress) => void): Promise<SessionsLoadResult> {
  if (isMockMode()) {
    const fixture = new URLSearchParams(location.search).get('fixture') === 'stress'
      ? stressSessionsFixture
      : sessionsFixture;
    const hosts = (fixture as SessionsFixture).hosts;
    const sessions = hosts.flatMap((host, hostIndex) =>
      host.sessions.map((session) => ({
        ...session,
        sourceHost: { id: host.id, name: host.name, isSelf: hostIndex === 0 },
      })),
    );
    const result = { sessions, hostsTotal: hosts.length, hostsSucceeded: hosts.length, failedHosts: [], unreachableHostIds: [] };
    onProgress?.({
      ...result,
      loadingHosts: [],
      knownHosts: hosts.map((host, hostIndex) => ({ id: host.id, name: host.name, isSelf: hostIndex === 0 })),
      succeededHostIds: hosts.map((host) => host.id),
    });
    return result;
  }

  const network = await getNetwork();
  const selfIp = network?.self.tailnetIPs?.[0] || network?.self.ip;
  const hosts: SessionHost[] = [
    {
      id: selfIp || location.hostname,
      name: network?.self.hostname || location.hostname,
      gatewayUrl: httpBase(),
      isSelf: true,
    },
    ...(network?.peers ?? [])
      .filter((peer) => peer.hasGateway === true && Boolean(peer.tailnetIPs?.[0]))
      .sort((a, b) => (a.hostname || a.tailnetIPs![0]).localeCompare(b.hostname || b.tailnetIPs![0]))
      .map((peer) => ({
        id: peer.tailnetIPs![0],
        name: peer.hostname || peer.tailnetIPs![0],
        gatewayUrl: gatewayUrlForIp(peer.tailnetIPs![0]),
        isSelf: false,
      })),
  ];

  const knownHostIds = new Set(hosts.map((host) => host.id));
  hostReachability.forEach((_, hostId) => { if (!knownHostIds.has(hostId)) hostReachability.delete(hostId); });

  let sessions: HostedSession[] = [];
  let hostsSucceeded = 0;
  const succeededHostIds = new Set<string>();
  const hostsToPoll = hosts.filter((host) => {
    const reachability = hostReachability.get(host.id);
    return !reachability?.unreachable || Date.now() >= reachability.nextAttemptAt;
  });
  const loadingHosts = new Map(hostsToPoll.map((host) => [host.id, host]));
  const reachabilitySnapshot = () => {
    const unreachableHosts = hosts.filter((host) => hostReachability.get(host.id)?.unreachable);
    return {
      failedHosts: unreachableHosts.map((host) => host.name),
      unreachableHostIds: unreachableHosts.map((host) => host.id),
    };
  };
  const reportProgress = () => {
    const reachability = reachabilitySnapshot();
    onProgress?.({
      sessions,
      hostsTotal: hosts.length,
      hostsSucceeded,
      ...reachability,
      loadingHosts: [...loadingHosts.values()],
      knownHosts: hosts,
      succeededHostIds: [...succeededHostIds],
    });
  };

  reportProgress();
  await Promise.all(hostsToPoll.map(async (host) => {
    try {
      const hostSessions = await fetchHostSessions(host);
      sessions = [
        ...sessions.filter((session) => session.sourceHost.id !== host.id),
        ...hostSessions,
      ];
      hostReachability.delete(host.id);
      hostsSucceeded += 1;
      succeededHostIds.add(host.id);
    } catch {
      const previous = hostReachability.get(host.id);
      const consecutiveFailures = (previous?.consecutiveFailures ?? 0) + 1;
      const unreachable = previous?.unreachable === true || consecutiveFailures >= HOST_FAILURE_THRESHOLD;
      hostReachability.set(host.id, {
        consecutiveFailures,
        unreachable,
        nextAttemptAt: unreachable ? Date.now() + UNREACHABLE_POLL_INTERVAL_MS : 0,
      });
    } finally {
      loadingHosts.delete(host.id);
      reportProgress();
    }
  }));
  const reachability = reachabilitySnapshot();
  if (hostsSucceeded === 0 && reachability.unreachableHostIds.length === 0) throw new Error('无法连接任何主机的 /api/sessions');
  return { sessions, hostsTotal: hosts.length, hostsSucceeded, ...reachability };
}

function sessionBase(session: HostedSession): string {
  return session.sourceHost.gatewayUrl || httpBase();
}

export async function getTimelinePage(
  session: HostedSession,
  options: { afterSeq?: number; beforeSeq?: number; limit?: number } = {},
  signal?: AbortSignal,
): Promise<TimelinePage> {
  const limit = options.limit ?? 50;
  if (isMockMode()) {
    if (new URLSearchParams(location.search).get('fixture') === 'deep' && options.afterSeq !== undefined) {
      await new Promise((resolve) => window.setTimeout(resolve, 200));
    }
    const key = Object.keys(timelineFixtures).find((path) => path.endsWith(`timeline-${session.id}.json`));
    const all = new URLSearchParams(location.search).get('fixture') === 'deep'
      ? deepTimelineFixture
      : key ? [...timelineFixtures[key]].sort((a, b) => a.seq - b.seq) : [];
    const eligible = options.beforeSeq === undefined ? all : all.filter((event) => event.seq < options.beforeSeq!);
    const events = options.afterSeq === undefined
      ? eligible.slice(-limit)
      : eligible.filter((event) => event.seq > options.afterSeq!).slice(0, limit);
    return { events, hasMoreBefore: options.afterSeq === undefined && eligible.length > events.length, nextBeforeSeq: events[0]?.seq ?? null };
  }
  if (isV2AdapterMode()) {
    const { getV2EntryTimelinePage, getV2RecordTimelinePage } = await import('./v2/liveData');
    if (session.v2?.target === 'entry' && session.v2.entryId) {
      return getV2EntryTimelinePage(session.sourceHost.id, session.v2.entryId, { ...options, limit }, signal);
    }
    if (session.v2?.target === 'record' && session.v2.recordId) {
      return getV2RecordTimelinePage(session.sourceHost.id, session.v2.recordId, { ...options, limit }, signal);
    }
    throw new Error('操作失败：v2 时间线目标无效');
  }

  const query = new URLSearchParams({ limit: String(limit) });
  if (options.afterSeq !== undefined) query.set('afterSeq', String(options.afterSeq));
  if (options.beforeSeq !== undefined) query.set('beforeSeq', String(options.beforeSeq));
  const response = await fetch(
    `${sessionBase(session)}/api/sessions/${encodeURIComponent(session.id)}/timeline?${query}`,
    { signal },
  );
  if (!response.ok) throw new Error(`GET timeline: HTTP ${response.status}`);
  const data = await readJson(response, 'GET timeline');
  const events = Array.isArray(data) ? data : isRecord(data) && Array.isArray(data.events) ? data.events : null;
  if (!events || !events.every(isTimelineEvent)) throw new Error('操作失败：GET timeline 响应结构错误');
  const sorted = events.sort((a, b) => a.seq - b.seq);
  return {
    events: sorted,
    hasMoreBefore: isRecord(data) && typeof data.hasMoreBefore === 'boolean' ? data.hasMoreBefore : sorted.length >= limit,
    nextBeforeSeq: isRecord(data) && (data.nextBeforeSeq === null || Number.isInteger(data.nextBeforeSeq)) ? data.nextBeforeSeq as number | null : sorted[0]?.seq ?? null,
  };
}

export async function getTimeline(session: HostedSession, afterSeq?: number, signal?: AbortSignal): Promise<TimelineEvent[]> {
  return (await getTimelinePage(session, { afterSeq }, signal)).events;
}

export interface SessionDeliveryAccepted {
  status: 'accepted';
  entryId: string;
  clientNonce: string;
  deliveryId: string;
}

export async function sendSessionMessage(session: HostedSession, text: string, nonce = clientNonce()): Promise<SessionDeliveryAccepted | void> {
  if (!session.canSend) throw new Error('该会话当前只读');
  if (isMockMode()) return;
  if (isV2AdapterMode()) {
    const { sendV2EntryMessage } = await import('./v2/liveData');
    try {
      return await sendV2EntryMessage(session.sourceHost.id, v2EntryId(session), text, nonce);
    } catch (error) {
      return rethrowV2WriteError(session, error);
    }
  }

  const response = await fetch(`${sessionBase(session)}/api/sessions/${encodeURIComponent(session.id)}/send`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text }),
  });
  if (response.status !== 200) {
    const body = await response.text();
    let reason = body.trim();
    const contentType = response.headers.get('content-type')?.split(';')[0].trim() ?? '';
    if (contentType === 'application/json' || contentType.endsWith('+json')) {
      try {
        const data = JSON.parse(body) as unknown;
        if (isRecord(data)) reason = typeof data.error === 'string' ? data.error : typeof data.message === 'string' ? data.message : reason;
      } catch {}
    }
    throw new SessionSendError(response.status, `HTTP ${response.status}${reason ? `：${reason}` : ''}`);
  }
}

export class SessionSendError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = 'SessionSendError';
  }
}

export async function chooseSessionOption(session: HostedSession, option: number, nonce = clientNonce()): Promise<void> {
  if (!session.canSend) throw new Error('该会话当前只读');
  if (isMockMode()) return;
  if (isV2AdapterMode()) {
    const { chooseV2EntryOption } = await import('./v2/liveData');
    try {
      await chooseV2EntryOption(session.sourceHost.id, v2EntryId(session), option, nonce);
      return;
    } catch (error) {
      return rethrowV2WriteError(session, error);
    }
  }

  const response = await fetch(`${sessionBase(session)}/api/sessions/${encodeURIComponent(session.id)}/choose`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ option }),
  });
  if (!response.ok) throw new Error(`POST choose: HTTP ${response.status}`);
}

export async function killSession(session: HostedSession, nonce = clientNonce()): Promise<void> {
  if (!session.canSend) throw new Error('该会话当前只读');
  if (isMockMode()) return;
  if (isV2AdapterMode()) {
    const { killV2Entry, refreshV2Host } = await import('./v2/liveData');
    try {
      await killV2Entry(session.sourceHost.id, v2EntryId(session), nonce);
      void import('./timelineCache').then(({ invalidateTimelineEntry }) => invalidateTimelineEntry(session.sourceHost.id, v2EntryId(session)));
      void refreshV2Host(session.sourceHost.id);
      return;
    } catch (error) {
      return rethrowV2WriteError(session, error);
    }
  }

  const response = await fetch(`${sessionBase(session)}/api/sessions/${encodeURIComponent(session.id)}/kill`, {
    method: 'POST',
  });
  if (response.status !== 200) throw new Error(`POST kill: HTTP ${response.status}`);
  const data = await readJson(response, 'POST kill');
  if (!isRecord(data)) throw new Error('操作失败：POST kill 响应结构错误');
  if (data.closed === true) return;
  // 兼容尚未返回 closed 的 gateway；契约以 HTTP 200 作为就地收敛依据。
}

export async function uploadSessionFile(
  session: HostedSession,
  file: File,
  onProgress: (percent: number) => void,
  nonce = clientNonce(),
): Promise<string> {
  if (!session.canSend) throw new Error('该会话当前只读');
  if (file.size > 20 * 1024 * 1024) throw new Error('附件不能超过 20MB');
  if (isMockMode()) {
    onProgress(100);
    return `/mock-uploads/${file.name}`;
  }
  if (isV2AdapterMode()) {
    const { uploadV2EntryFile } = await import('./v2/liveData');
    try {
      return (await uploadV2EntryFile(session.sourceHost.id, v2EntryId(session), file, nonce, onProgress)).path;
    } catch (error) {
      return rethrowV2WriteError(session, error);
    }
  }

  return new Promise((resolve, reject) => {
    const request = new XMLHttpRequest();
    request.open('POST', `${sessionBase(session)}/api/sessions/${encodeURIComponent(session.id)}/upload`);
    request.upload.onprogress = (event) => {
      if (event.lengthComputable) onProgress(Math.round((event.loaded / event.total) * 100));
    };
    request.onerror = () => reject(new Error('附件上传失败'));
    request.onload = () => {
      if (request.status < 200 || request.status >= 300) {
        reject(new Error(`POST upload: HTTP ${request.status}`));
        return;
      }
      const contentType = request.getResponseHeader('Content-Type')?.split(';')[0].trim() ?? '';
      if (contentType !== 'application/json' && !contentType.endsWith('+json')) {
        reject(new Error(`POST upload: 响应格式错误（${contentType || '未知格式'}，不是 JSON）`));
        return;
      }
      try {
        const data = JSON.parse(request.responseText) as unknown;
        if (!isRecord(data) || typeof data.path !== 'string' || !data.path.startsWith('/')) throw new Error('POST upload: 响应结构错误（缺少绝对路径 path）');
        onProgress(100);
        resolve(data.path);
      } catch (error) {
        reject(error instanceof Error ? error : new Error(String(error)));
      }
    };
    const body = new FormData();
    body.append('file', file);
    request.send(body);
  });
}

export interface SessionStateUpdate {
  state?: SessionState;
  canSend?: boolean;
  live?: boolean;
  lastActivityAt?: string;
  lastMessagePreview?: string;
}

interface StreamHandlers {
  onOpen: () => void;
  onTimeline: (event: TimelineEvent) => void;
  onState: (state: SessionStateUpdate) => void;
  onDelivery?: (delivery: { entryId: string; deliveryId: string; clientNonce: string; status: 'accepted' | 'echoed' | 'unattributed'; recordId?: string | null }) => void;
  onError: () => void;
}

export function subscribeSessionStream(session: HostedSession, handlers: StreamHandlers, afterSeq = 0): () => void {
  if (isMockMode()) {
    queueMicrotask(handlers.onOpen);
    return () => {};
  }
  if (isV2AdapterMode()) {
    if (session.v2?.target !== 'entry' || !session.v2.entryId) {
      queueMicrotask(handlers.onOpen);
      return () => {};
    }
    let cancelled = false;
    let unsubscribe = () => {};
    const { sourceHost, v2 } = session;
    void import('./v2/liveData').then(({ refreshV2Host, subscribeV2EntryStream }) => {
      if (cancelled) return;
      unsubscribe = subscribeV2EntryStream(sourceHost.id, v2.entryId!, afterSeq, {
        onOpen: handlers.onOpen,
        onTimeline: handlers.onTimeline,
        onState: (state) => handlers.onState({ state: state === 'unknown' ? 'idle' : state }),
        onDelivery: (delivery) => handlers.onDelivery?.(delivery),
        onAttachmentChanged: (attachmentRevision) => {
          if (attachmentRevision > v2.attachmentRevision) {
            void import('./timelineCache').then(({ invalidateTimelineEntry }) => invalidateTimelineEntry(sourceHost.id, v2.entryId!));
            void refreshV2Host(sourceHost.id);
          }
        },
        onEntryRemoved: () => {
          void import('./timelineCache').then(({ invalidateTimelineEntry }) => invalidateTimelineEntry(sourceHost.id, v2.entryId!));
          void refreshV2Host(sourceHost.id);
        },
        onSnapshotRequired: () => { void refreshV2Host(sourceHost.id); },
        onError: handlers.onError,
      });
    });
    return () => {
      cancelled = true;
      unsubscribe();
    };
  }

  const source = new EventSource(`${sessionBase(session)}/api/sessions/${encodeURIComponent(session.id)}/stream`);
  source.onopen = handlers.onOpen;
  source.addEventListener('timeline', (event) => {
    try { handlers.onTimeline(JSON.parse((event as MessageEvent<string>).data) as TimelineEvent); } catch {}
  });
  source.addEventListener('state', (event) => {
    try { handlers.onState(JSON.parse((event as MessageEvent<string>).data) as SessionStateUpdate); } catch {}
  });
  source.onerror = handlers.onError;
  return () => source.close();
}

export interface NetworkPeer {
  hostname: string;
  tailnetIPs?: string[];
  os?: string;
  online?: boolean;
  hasGateway?: boolean;
  ip?: string;
}

export interface NetworkSelf {
  hostname: string;
  os?: string;
  tailnetIPs?: string[];
  tsnetOn?: boolean;
  ip?: string;
}

export interface NetworkInfo {
  self: NetworkSelf;
  peers: NetworkPeer[];
  mode?: string;
  note?: string;
  inTailnet?: boolean;
}

function isNetworkInfo(value: unknown): value is NetworkInfo {
  return isRecord(value)
    && isRecord(value.self)
    && typeof value.self.hostname === 'string'
    && Array.isArray(value.peers)
    && value.peers.every((peer) => isRecord(peer) && typeof peer.hostname === 'string');
}

// 网络发现始终同源，避免选中节点不可达时无法恢复。
export async function getNetwork(): Promise<NetworkInfo | null> {
  try {
    const response = await fetch(`${location.protocol}//${location.host}/api/network`);
    if (!response.ok) return null;
    const data = await readJson(response, 'GET network');
    return isNetworkInfo(data) ? data : null;
  } catch {
    return null;
  }
}
