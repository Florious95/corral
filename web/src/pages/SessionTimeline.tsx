import { Fragment, memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import {
  chooseSessionOption,
  sendSessionMessage,
  sessionCanWrite,
  SessionSendError,
  subscribeSessionStream,
  uploadSessionFile,
  type SessionState,
  type SessionStateUpdate,
  type TimelineEvent,
} from '../api';
import { Markdown } from '../components/Markdown';
import { KillSessionDialog } from '../components/KillSessionDialog';
import { TerminalScreen } from '../components/TerminalScreen';
import { findFavoriteSnapshot } from '../favorites';
import { refreshSessions, updateSessionState, useSessions } from '../hooks/useSessions';
import { cachedTimeline, cachedTimelinePageInfo, hydrateTimelineCache, isRetryableTimelineError, loadTimelineAfter, loadTimelineBefore, loadTimelinePage, rememberTimelineEvents } from '../timelineCache';

type ToolUse = Extract<TimelineEvent, { type: 'tool_use' }>;
type ToolResult = Extract<TimelineEvent, { type: 'tool_result' }>;
type SkillLoad = Extract<TimelineEvent, { type: 'skill_load' }>;
interface ToolItem {
  kind: 'tool';
  seq: number;
  ts: string;
  use?: ToolUse;
  result?: ToolResult;
}
type TaskStatus = 'pending' | 'in_progress' | 'completed';
interface TaskEntry {
  id: string;
  subject: string;
  status: TaskStatus;
}
interface TaskListItem {
  kind: 'tasks';
  seq: number;
  ts: string;
  tasks: TaskEntry[];
}
interface PendingAttachment {
  id: string;
  clientNonce: string;
  file: File;
  preview?: string;
  status: 'pending' | 'uploading' | 'uploaded' | 'error';
  progress: number;
  path?: string;
  error?: string;
}
interface PendingMessage {
  id: string;
  deliveryId?: string;
  text: string;
  ts: string;
  status: 'sending' | 'sent' | 'submitting' | 'accepted' | 'echoed' | 'unattributed' | 'failed';
}

function attachmentId(): string {
  if (typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  return `attachment-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

const DELIVERY_RANK: Record<PendingMessage['status'], number> = {
  sending: 0,
  submitting: 0,
  failed: 0,
  sent: 1,
  accepted: 1,
  echoed: 2,
  unattributed: 2,
};

const MAX_TIMELINE_DOM_ITEMS = 60;
const TIMELINE_WINDOW_STEP = 30;

function isRcApp(): boolean {
  return typeof navigator !== 'undefined' && /rcapp/i.test(navigator.userAgent);
}

function useRcAppVisualViewport(enabled: boolean, shellRef: RefObject<HTMLElement | null>, timelineRef: RefObject<HTMLDivElement | null>) {
  useEffect(() => {
    const shell = shellRef.current;
    const viewport = window.visualViewport;
    if (!enabled || !shell || !viewport) return;
    let frame = 0;
    let baseHeight = Math.max(window.innerHeight, viewport.height + viewport.offsetTop);
    const sync = () => {
      frame = 0;
      const visibleBottom = viewport.height + viewport.offsetTop;
      const offset = Math.max(0, baseHeight - visibleBottom);
      if (offset < 1) baseHeight = Math.max(window.innerHeight, visibleBottom);
      const scrollTop = timelineRef.current?.scrollTop;
      shell.style.setProperty('--rcapp-layout-height', `${Math.round(baseHeight)}px`);
      shell.style.setProperty('--rcapp-composer-y', `${-Math.round(offset)}px`);
      if (scrollTop !== undefined && timelineRef.current) timelineRef.current.scrollTop = scrollTop;
    };
    const schedule = () => {
      if (!frame) frame = requestAnimationFrame(sync);
    };
    const reset = () => {
      baseHeight = Math.max(window.innerHeight, viewport.height + viewport.offsetTop);
      schedule();
    };
    sync();
    viewport.addEventListener('resize', schedule, { passive: true });
    viewport.addEventListener('scroll', schedule, { passive: true });
    window.addEventListener('orientationchange', reset, { passive: true });
    return () => {
      if (frame) cancelAnimationFrame(frame);
      viewport.removeEventListener('resize', schedule);
      viewport.removeEventListener('scroll', schedule);
      window.removeEventListener('orientationchange', reset);
      shell.style.removeProperty('--rcapp-layout-height');
      shell.style.removeProperty('--rcapp-composer-y');
    };
  }, [enabled, shellRef, timelineRef]);
}

function deliveryLabel(status: PendingMessage['status']): string {
  if (status === 'submitting') return '正在提交…';
  if (status === 'accepted') return '发送中';
  if (status === 'echoed') return '已送达';
  if (status === 'unattributed') return '已发送 · 附着待确认';
  if (status === 'failed') return '发送失败';
  if (status === 'sending') return '发送中…';
  return '已发送';
}
type RenderItem = Exclude<TimelineEvent, ToolUse | ToolResult> | ToolItem | TaskListItem;

function comparableMessageText(text: string): string {
  return text
    .replace(/\r\n?/g, '\n')
    .split('\n')
    .map((line) => attachmentPathFromLine(line) ?? line)
    .join('\n')
    .trim();
}

function attachmentPathFromLine(line: string): string | null {
  const trimmed = line.trim();
  const path = trimmed.startsWith('@/') ? trimmed.slice(1) : trimmed;
  return path.startsWith('/') && path.includes('/corral-uploads/') ? path : null;
}

export function UserMessageContent({ text }: { text: string }) {
  const body: string[] = [];
  const attachments: string[] = [];
  text.replace(/\r\n?/g, '\n').split('\n').forEach((line) => {
    const path = attachmentPathFromLine(line);
    if (path) attachments.push(path);
    else body.push(line);
  });
  const markdown = body.join('\n').trim();
  return (
    <>
      {markdown && <Markdown text={markdown} />}
      {attachments.map((path, index) => (
        <div className="timeline-attachment-card" data-qa="timeline-attachment-card" data-path={path} key={`${path}:${index}`}>
          <span className="timeline-attachment-icon" aria-hidden="true">📎</span>
          <span className="timeline-attachment-copy">
            <span>{path.split('/').at(-1)}</span>
            <small>已随消息发送</small>
          </span>
        </div>
      ))}
    </>
  );
}

function reconcilePendingMessages(messages: PendingMessage[], incoming: TimelineEvent[]): PendingMessage[] {
  const remaining = [...messages];
  incoming.forEach((event) => {
    if (event.type !== 'user_message') return;
    const match = remaining.findIndex((message) => comparableMessageText(message.text) === comparableMessageText(event.text));
    if (match >= 0) remaining.splice(match, 1);
  });
  return remaining;
}

function timelineDisplayItems(items: RenderItem[], pending: PendingMessage[]) {
  const display: Array<{ type: 'event'; item: RenderItem } | { type: 'pending'; message: PendingMessage }> = items.map((item) => ({ type: 'event', item }));
  [...pending]
    .sort((left, right) => Date.parse(left.ts) - Date.parse(right.ts))
    .forEach((message) => {
      const pendingAt = Date.parse(message.ts);
      const insertAt = display.findIndex((entry) => entry.type === 'event' && Date.parse(entry.item.ts) > pendingAt);
      display.splice(insertAt < 0 ? display.length : insertAt, 0, { type: 'pending', message });
    });
  return display;
}

function mergeEvents(current: TimelineEvent[], incoming: TimelineEvent[]): TimelineEvent[] {
  const bySeq = new Map(current.map((event) => [event.seq, event]));
  incoming.forEach((event) => bySeq.set(event.seq, event));
  return [...bySeq.values()].sort((a, b) => a.seq - b.seq);
}

function timelineItems(events: TimelineEvent[]): RenderItem[] {
  const items: Array<Exclude<TimelineEvent, ToolUse | ToolResult> | ToolItem> = [];
  const pending = new Map<string, ToolItem[]>();

  for (const event of events) {
    if (event.type === 'tool_use') {
      const item: ToolItem = { kind: 'tool', seq: event.seq, ts: event.ts, use: event };
      items.push(item);
      pending.set(event.tool, [...(pending.get(event.tool) ?? []), item]);
    } else if (event.type === 'tool_result') {
      const queue = pending.get(event.tool) ?? [];
      const item = queue.shift();
      if (item) item.result = event;
      else items.push({ kind: 'tool', seq: event.seq, ts: event.ts, result: event });
      pending.set(event.tool, queue);
    } else {
      items.push(event);
    }
  }
  const pendingSkillLoads = new Map<string, number>();
  items.forEach((item) => {
    if (!('type' in item) || item.type !== 'skill_load') return;
    pendingSkillLoads.set(item.skill, (pendingSkillLoads.get(item.skill) ?? 0) + 1);
  });
  const tasks = new Map<string, TaskEntry>();
  const visible: RenderItem[] = [];
  for (const item of items) {
    if ('kind' in item && item.kind === 'tool' && item.use?.tool === 'Skill') {
      const input = parseToolInput(item.use.input);
      const skill = typeof input?.skill === 'string' ? input.skill : '';
      const remaining = pendingSkillLoads.get(skill) ?? 0;
      if (skill && remaining > 0) {
        pendingSkillLoads.set(skill, remaining - 1);
        continue;
      }
    }
    if (!('kind' in item) || item.kind !== 'tool' || !item.use || !['TaskCreate', 'TaskUpdate', 'TodoWrite'].includes(item.use.tool)) {
      visible.push(item);
      continue;
    }
    const input = parseToolInput(item.use.input);
    const rawInput = item.use.input ?? '';
    if (!input) {
      tasks.set(`raw:${item.seq}`, { id: `raw:${item.seq}`, subject: rawInput, status: 'pending' });
    } else if (item.use.tool === 'TaskCreate') {
      const resultId = item.result?.output.match(/Task #(\S+) created/i)?.[1];
      const id = String(resultId ?? input.taskId ?? input.id ?? `create:${item.seq}`);
      const subject = typeof input.subject === 'string' && input.subject ? input.subject : rawInput;
      tasks.set(id, { id, subject, status: normalizeTaskStatus(input.status) });
    } else if (item.use.tool === 'TaskUpdate') {
      const id = String(input.taskId ?? input.id ?? `update:${item.seq}`);
      const previous = tasks.get(id);
      const subject = typeof input.subject === 'string' && input.subject ? input.subject : previous?.subject ?? rawInput;
      tasks.set(id, { id, subject, status: input.status === undefined ? previous?.status ?? 'pending' : normalizeTaskStatus(input.status) });
    } else if (Array.isArray(input.todos)) {
      input.todos.forEach((todo, index) => {
        if (!todo || typeof todo !== 'object') return;
        const record = todo as Record<string, unknown>;
        const content = typeof record.content === 'string' && record.content ? record.content : '';
        const subject = content || rawInput;
        const id = String(record.id ?? (content || `todo:${item.seq}:${index}`));
        tasks.set(id, { id, subject, status: normalizeTaskStatus(record.status) });
      });
    } else {
      tasks.set(`raw:${item.seq}`, { id: `raw:${item.seq}`, subject: rawInput, status: 'pending' });
    }
    visible.push({ kind: 'tasks', seq: item.seq, ts: item.ts, tasks: [...tasks.values()] });
  }
  return visible.sort((a, b) => a.seq - b.seq);
}

function normalizeTaskStatus(status: unknown): TaskStatus {
  if (status === 'completed' || status === 'done') return 'completed';
  if (status === 'in_progress' || status === 'doing') return 'in_progress';
  return 'pending';
}

function time(ts: string): string {
  return new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false }).format(new Date(ts));
}

function formatEventAge(ts: string, now: number): string {
  const elapsedMinutes = Math.max(0, Math.floor((now - new Date(ts).getTime()) / 60_000));
  if (elapsedMinutes < 60) return `${elapsedMinutes} 分钟前`;
  if (elapsedMinutes < 24 * 60) return `${Math.floor(elapsedMinutes / 60)} 小时 ${elapsedMinutes % 60} 分钟前`;
  return `${Math.floor(elapsedMinutes / (24 * 60))} 天 ${Math.floor((elapsedMinutes % (24 * 60)) / 60)} 小时前`;
}

const SESSION_STATE_LABEL: Record<SessionState, string> = {
  running: '运行中',
  waiting_input: '等待输入',
  idle: '空闲',
  gone: '已结束',
};

function parseToolInput(input?: string): Record<string, unknown> | null {
  if (!input) return null;
  try {
    const parsed = JSON.parse(input) as unknown;
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed as Record<string, unknown> : null;
  } catch {
    return null;
  }
}

function shellParts(command: string): ReactNode[] {
  const token = /(--?[\w-]+|"(?:\\.|[^"])*"|'(?:\\.|[^'])*'|\b\d+\b)/g;
  return command.split(token).filter(Boolean).map((part, index) => {
    const className = part.startsWith('-')
      ? 'shell-flag'
      : /^['"]/.test(part)
        ? 'code-string'
        : /^\d+$/.test(part)
          ? 'code-number'
          : undefined;
    return <span className={className} key={index}>{part}</span>;
  });
}

function ToolFields({ input }: { input: Record<string, unknown> }) {
  const fields = Object.entries(input).filter(([key]) => key !== 'description' && key !== 'command');
  if (fields.length === 0) return null;
  return (
    <dl className="tool-fields">
      {fields.map(([key, value]) => {
        const text = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
        return (
          <Fragment key={key}>
            <dt>{key}</dt>
            <dd>{text.includes('\n') ? <pre>{text}</pre> : <code>{text}</code>}</dd>
          </Fragment>
        );
      })}
    </dl>
  );
}

function ToolCard({ item }: { item: ToolItem }) {
  const tool = item.use?.tool ?? item.result?.tool ?? 'Tool';
  const input = parseToolInput(item.use?.input);
  const command = typeof input?.command === 'string' ? input.command : '';
  const description = typeof input?.description === 'string' ? input.description : item.use?.summary?.trim();
  const skillName = tool === 'Skill' && typeof input?.skill === 'string' ? input.skill : '';
  const summary = skillName
    ? `加载 Skill: ${skillName}`
    : item.use?.summary?.trim() || (command ? command.slice(0, 60) : '执行了一项工具调用');
  const shellTool = /^(Bash|Shell|Terminal)$/i.test(tool);
  return (
    <details className="tool-row" data-qa="tool-card" data-seq={item.seq}>
      <summary>
        <span className="tool-summary">{summary}</span>
        <span className="tool-caret">›</span>
      </summary>
      <div className="tool-detail">
        <div className="tool-detail-section">
          <div className="tool-section-label">工具</div>
          <div className="tool-name">{tool}</div>
        </div>
        {item.use?.input && (
          <div className="tool-detail-section">
            <div className="tool-section-label">输入</div>
            {description && <div className="tool-description">{description}</div>}
            {shellTool && command
              ? <pre className="tool-shell"><code><span className="shell-prompt">$</span> {shellParts(command)}</code></pre>
              : input
                ? <ToolFields input={input} />
                : <div className="tool-input-text">{item.use.input}</div>}
          </div>
        )}
        {item.result && (
          <div className="tool-detail-section">
            <div className="tool-section-label">输出 · {item.result.ok ? '成功' : '失败'}{item.result.truncated ? ' · 已截断' : ''}</div>
            <pre>{item.result.output}</pre>
          </div>
        )}
      </div>
    </details>
  );
}

function SkillLoadRow({ item }: { item: SkillLoad }) {
  return (
    <details className="tool-row skill-load-row" data-qa="skill-load" data-seq={item.seq}>
      <summary>
        <span className="tool-summary">加载 Skill: {item.skill}</span>
        <span className="tool-caret">›</span>
      </summary>
      <div className="tool-detail">
        <div className="tool-detail-section">
          <Markdown text={item.text} />
        </div>
      </div>
    </details>
  );
}

const TASK_STATUS_COPY: Record<TaskStatus, { glyph: string; label: string }> = {
  pending: { glyph: '□', label: '未开始' },
  in_progress: { glyph: '■', label: '进行中' },
  completed: { glyph: '✔', label: '已完成' },
};

function TaskList({ item }: { item: TaskListItem }) {
  return (
    <section className="task-list" data-qa="task-list" data-seq={item.seq}>
      <div className="task-list-title">任务</div>
      {item.tasks.map((task) => {
        const copy = TASK_STATUS_COPY[task.status];
        return (
          <div className={`task-item status-${task.status}`} data-qa="task-item" data-task-id={task.id} data-task-status={task.status} key={task.id}>
            <span className="task-glyph" aria-hidden="true">{copy.glyph}</span>
            <span className="task-subject">{task.subject}</span>
            <span className="task-status-label">{copy.label}</span>
          </div>
        );
      })}
    </section>
  );
}

function AskUserQuestion({ item, canChoose, onChoose }: { item: ToolItem; canChoose: boolean; onChoose: (option: number) => Promise<void> }) {
  const [choosing, setChoosing] = useState<number | null>(null);
  const [chosen, setChosen] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const input = parseToolInput(item.use?.input);
  const question = Array.isArray(input?.questions) ? input.questions[0] : null;
  if (!question || typeof question !== 'object') return <ToolCard item={item} />;
  const record = question as Record<string, unknown>;
  const prompt = typeof record.question === 'string' ? record.question : '';
  const options = Array.isArray(record.options) ? record.options : [];
  if (!prompt || options.length === 0) return <ToolCard item={item} />;

  const choose = async (option: number) => {
    setChoosing(option);
    setError(null);
    try {
      await onChoose(option);
      setChosen(option);
    } catch (nextError) {
      setError(nextError instanceof Error ? nextError.message : String(nextError));
    } finally {
      setChoosing(null);
    }
  };

  return (
    <section className="ask-question" data-qa="ask-user-question" data-seq={item.seq}>
      {typeof record.header === 'string' && <div className="ask-header">{record.header}</div>}
      <div className="ask-prompt">{prompt}</div>
      <div className="ask-options">
        {options.map((option, index) => {
          if (!option || typeof option !== 'object') return null;
          const optionRecord = option as Record<string, unknown>;
          const label = typeof optionRecord.label === 'string' ? optionRecord.label : `选项 ${index + 1}`;
          const description = typeof optionRecord.description === 'string' ? optionRecord.description : '';
          return (
            <button
              data-qa="ask-option"
              data-option={index + 1}
              className={chosen === index + 1 ? 'selected' : ''}
              type="button"
              disabled={!canChoose || choosing !== null || chosen !== null}
              onClick={() => choose(index + 1)}
              key={index}
            >
              <span>{choosing === index + 1 ? '正在选择…' : label}</span>
              {description && <small>{description}</small>}
            </button>
          );
        })}
      </div>
      {!canChoose && <div className="ask-note">当前会话只读，无法选择</div>}
      {chosen !== null && <div className="ask-note success">已提交选择</div>}
      {error && <div className="ask-note error">{error}</div>}
    </section>
  );
}

const Event = memo(function Event({ item, canChoose, onChoose }: { item: RenderItem; canChoose: boolean; onChoose: (option: number, seq: number) => Promise<void> }) {
  if ('kind' in item) {
    if (item.kind === 'tasks') return <TaskList item={item} />;
    if (item.use?.tool === 'AskUserQuestion' && !item.result) return <AskUserQuestion item={item} canChoose={canChoose} onChoose={(option) => onChoose(option, item.seq)} />;
    return <ToolCard item={item} />;
  }
  if (item.type === 'skill_load') return <SkillLoadRow item={item} />;
  if (item.type === 'status') return <div className="timeline-status" data-qa="status-event">{item.text}</div>;
  return (
    <div className={`message-row ${item.type === 'user_message' ? 'user' : 'assistant'}`} data-qa={`${item.type}-bubble`} data-seq={item.seq}>
      <div className="message-bubble">
        {item.type === 'user_message' ? <UserMessageContent text={item.text} /> : <Markdown text={item.text} />}
        <time>{time(item.ts)}</time>
      </div>
    </div>
  );
});

export function ReadOnlyTimelineEvents({
  events,
  canChoose = false,
  onChoose = async () => {},
}: {
  events: TimelineEvent[];
  canChoose?: boolean;
  onChoose?: (option: number, seq: number) => Promise<void>;
}) {
  const items = useMemo(() => timelineItems(events), [events]);
  return items.map((item) => (
    <Event
      key={`${'kind' in item ? 'tool' : item.type}:${item.seq}`}
      item={item}
      canChoose={canChoose}
      onChoose={onChoose}
    />
  ));
}

export function SessionTimeline({
  embedded = false,
  hostId: hostIdOverride,
  sessionId: sessionIdOverride,
  onClose,
}: {
  embedded?: boolean;
  hostId?: string;
  sessionId?: string;
  onClose?: () => void;
}) {
  const navigate = useNavigate();
  const location = useLocation();
  const params = useParams();
  const hostId = hostIdOverride ?? params.hostId ?? '';
  const id = sessionIdOverride ?? params.id ?? '';
  const { sessions, loading: sessionsLoading, lastFetched, err: sessionsError } = useSessions();
  const exactApiSession = sessions.find((item) => item.sourceHost.id === hostId && item.id === id);
  const routePaneRef = useRef(exactApiSession?.v2?.paneId);
  if (exactApiSession?.v2?.paneId) routePaneRef.current = exactApiSession.v2.paneId;
  const reboundSession = !exactApiSession && routePaneRef.current
    ? sessions.find((item) => item.sourceHost.id === hostId && item.v2?.paneId === routePaneRef.current)
    : undefined;
  const apiSession = exactApiSession ?? reboundSession;
  const session = apiSession ?? findFavoriteSnapshot(hostId, id);
  const expired = Boolean(session && !apiSession && !sessionsLoading && lastFetched > 0);
  const baseWritable = Boolean(apiSession && sessionCanWrite(apiSession));
  const sessionRef = useRef(apiSession);
  sessionRef.current = apiSession;
  const initialTimeline = session ? cachedTimeline(session) : undefined;
  const [events, setEvents] = useState<TimelineEvent[]>(initialTimeline ?? []);
  const [loading, setLoading] = useState(!initialTimeline);
  const initialPageInfo = session ? cachedTimelinePageInfo(session) : undefined;
  const [hasMoreBefore, setHasMoreBefore] = useState(initialPageInfo?.hasMoreBefore ?? false);
  const [nextBeforeSeq, setNextBeforeSeq] = useState<number | null>(initialPageInfo?.nextBeforeSeq ?? null);
  const [loadingEarlier, setLoadingEarlier] = useState(false);
  const [cacheRefreshing, setCacheRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [input, setInput] = useState('');
  const [sending, setSending] = useState(false);
  const [killOpen, setKillOpen] = useState(false);
  const [attachmentMenuOpen, setAttachmentMenuOpen] = useState(false);
  const [attachments, setAttachments] = useState<PendingAttachment[]>([]);
  const [sendError, setSendError] = useState<string | null>(null);
  const [recovering, setRecovering] = useState(false);
  const [streamState, setStreamState] = useState<'connecting' | 'live' | 'reconnecting'>('connecting');
  const [localSuspect, setLocalSuspect] = useState(false);
  const [pending, setPending] = useState<PendingMessage[]>([]);
  const [timelineWindowStart, setTimelineWindowStart] = useState(0);
  const [contentView, setContentView] = useState<'timeline' | 'terminal'>('timeline');
  const writable = baseWritable && (!apiSession?.v2 || streamState === 'live');
  const composerAvailable = baseWritable;
  const terminalAvailable = apiSession?.v2?.target === 'entry' && Boolean(apiSession.v2.entryId);
  const rcapp = isRcApp() && !embedded;
  const shellRef = useRef<HTMLElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const cameraInputRef = useRef<HTMLInputElement>(null);
  const galleryInputRef = useRef<HTMLInputElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const attachmentsRef = useRef(attachments);
  attachmentsRef.current = attachments;
  const latestSeqRef = useRef(0);
  const backfillInFlightRef = useRef(false);
  const preserveScrollRef = useRef(false);
  const anchorRestoreRef = useRef<{ seq: string; top: number } | null>(null);
  const loadingOlderRef = useRef(false);
  const skipAutoScrollRef = useRef(false);
  const historyScrollArmedRef = useRef(false);
  const sendNonceRef = useRef<{ text: string; clientNonce: string } | null>(null);
  const chooseNoncesRef = useRef(new Map<string, string>());
  const items = useMemo(() => timelineItems(events), [events]);
  const displayItems = useMemo(() => timelineDisplayItems(items, pending), [items, pending]);
  const renderedDisplayItems = useMemo(
    () => displayItems.slice(timelineWindowStart, timelineWindowStart + MAX_TIMELINE_DOM_ITEMS),
    [displayItems, timelineWindowStart],
  );
  const previousDisplayLengthRef = useRef(displayItems.length);
  const latestEventAt = events.at(-1)?.ts ?? session?.lastActivityAt ?? '';
  const [ageNow, setAgeNow] = useState(() => Date.now());
  useRcAppVisualViewport(rcapp, shellRef, scrollRef);

  const chooseTimelineOption = useCallback((option: number, seq: number) => {
    if (!session) return Promise.reject(new Error('找不到会话'));
    const key = `${seq}:${option}`;
    const clientNonce = chooseNoncesRef.current.get(key) ?? attachmentId();
    chooseNoncesRef.current.set(key, clientNonce);
    return chooseSessionOption(session, option, clientNonce);
  }, [session]);

  useEffect(() => {
    if (!embedded && reboundSession && reboundSession.id !== id) {
      navigate(`/sessions/${encodeURIComponent(hostId)}/${encodeURIComponent(reboundSession.id)}${location.search}`, { replace: true });
    }
  }, [embedded, hostId, id, location.search, navigate, reboundSession]);

  useEffect(() => {
    setAgeNow(Date.now());
    const timer = window.setInterval(() => setAgeNow(Date.now()), 60_000);
    return () => window.clearInterval(timer);
  }, [latestEventAt]);

  useEffect(() => {
    const currentSession = sessionRef.current;
    if (!currentSession) {
      setLoading(false);
      setError(null);
      setEvents([]);
      setPending([]);
      return;
    }
    let alive = true;
    let unsubscribe = () => {};
    let timelineTimer: number | undefined;
    let retryDelay = 250;
    const cached = cachedTimeline(currentSession);
    const cachedInfo = cachedTimelinePageInfo(currentSession);
    setLoading(!cached);
    setCacheRefreshing(Boolean(cached));
    setError(null);
    setInput('');
    setSending(false);
    attachmentsRef.current.forEach((attachment) => { if (attachment.preview) URL.revokeObjectURL(attachment.preview); });
    setAttachments([]);
    setAttachmentMenuOpen(false);
    setSendError(null);
    setPending([]);
    setTimelineWindowStart(Math.max(0, timelineItems(cached ?? []).length - MAX_TIMELINE_DOM_ITEMS));
    setLocalSuspect(false);
    sendNonceRef.current = null;
    chooseNoncesRef.current.clear();
    setStreamState('connecting');
    latestSeqRef.current = cached?.at(-1)?.seq ?? 0;
    backfillInFlightRef.current = !cached;
    setEvents(cached ?? []);
    setHasMoreBefore(cachedInfo?.hasMoreBefore ?? false);
    setNextBeforeSeq(cachedInfo?.nextBeforeSeq ?? cached?.[0]?.seq ?? null);
    setLoadingEarlier(false);
    preserveScrollRef.current = false;
    historyScrollArmedRef.current = false;
    const scheduleTimelineLoad = (incremental: boolean, delay = 0) => {
      timelineTimer = window.setTimeout(() => {
        timelineTimer = undefined;
        if (!alive) return;
        backfillInFlightRef.current = true;
        const request = incremental
          ? loadTimelineAfter(currentSession, latestSeqRef.current)
          : loadTimelinePage(currentSession);
        request.then((next) => {
          if (!alive) return;
          retryDelay = 250;
          latestSeqRef.current = Math.max(latestSeqRef.current, next.at(-1)?.seq ?? 0);
          setEvents((current) => mergeEvents(current, next));
          const info = cachedTimelinePageInfo(currentSession);
          if (!incremental && info) {
            setHasMoreBefore(info.hasMoreBefore);
            setNextBeforeSeq(info.nextBeforeSeq);
          }
          setPending((messages) => reconcilePendingMessages(messages, next));
          backfillInFlightRef.current = false;
          setLoading(false);
          setCacheRefreshing(false);
        }).catch((nextError) => {
          if (!alive) return;
          if (isRetryableTimelineError(nextError)) {
            setStreamState('reconnecting');
            setError(null);
            scheduleTimelineLoad(incremental, retryDelay);
            retryDelay = Math.min(retryDelay * 2, 4_000);
            return;
          }
          backfillInFlightRef.current = false;
          setLoading(false);
          setCacheRefreshing(false);
          setError(nextError instanceof Error ? nextError.message : String(nextError));
        });
      }, delay);
    };
    const streamHandlers = {
      onOpen: () => { if (alive) setStreamState('live'); },
      onTimeline: (event: TimelineEvent) => {
        if (!alive) return;
        latestSeqRef.current = Math.max(latestSeqRef.current, event.seq);
        rememberTimelineEvents(currentSession, [event]);
        setEvents((current) => mergeEvents(current, [event]));
        if (event.type === 'user_message') {
          setPending((messages) => reconcilePendingMessages(messages, [event]));
        }
      },
      onState: (state: SessionStateUpdate) => {
        if (alive) updateSessionState(currentSession.sourceHost.id, currentSession.id, state);
      },
      onDelivery: (delivery: { entryId: string; deliveryId: string; clientNonce: string; status: 'accepted' | 'echoed' | 'unattributed'; recordId?: string | null }) => {
        if (!alive) return;
        setPending((messages) => messages.map((message) => {
          if (message.id !== delivery.clientNonce) return message;
          if (message.deliveryId && message.deliveryId !== delivery.deliveryId) return message;
          if (DELIVERY_RANK[delivery.status] < DELIVERY_RANK[message.status]) return message;
          return { ...message, deliveryId: delivery.deliveryId, status: delivery.status };
        }));
        if (delivery.status === 'unattributed') {
          setLocalSuspect(true);
          void refreshSessions();
        }
      },
      onError: () => {
        if (!alive) return;
        setStreamState('reconnecting');
        if (backfillInFlightRef.current) return;
        scheduleTimelineLoad(true);
      },
    };
    const openStream = (afterSeq: number) => {
      if (!alive || !currentSession.live) return;
      unsubscribe = subscribeSessionStream(currentSession, streamHandlers, afterSeq);
    };
    openStream(latestSeqRef.current);
    if (cached) {
      backfillInFlightRef.current = false;
      setLoading(false);
      scheduleTimelineLoad(true);
    } else {
      void hydrateTimelineCache(currentSession).then((persisted) => {
        if (!alive) return;
        if (!persisted) {
          scheduleTimelineLoad(false);
          return;
        }
        latestSeqRef.current = Math.max(latestSeqRef.current, persisted.events.at(-1)?.seq ?? 0);
        setEvents((current) => mergeEvents(current, persisted.events));
        setHasMoreBefore(persisted.pageInfo.hasMoreBefore);
        setNextBeforeSeq(persisted.pageInfo.nextBeforeSeq);
        setTimelineWindowStart(Math.max(0, timelineItems(persisted.events).length - MAX_TIMELINE_DOM_ITEMS));
        backfillInFlightRef.current = false;
        setLoading(false);
        setCacheRefreshing(true);
        scheduleTimelineLoad(true);
      }).catch(() => {
        if (alive) scheduleTimelineLoad(false);
      });
    }
    return () => {
      alive = false;
      if (timelineTimer !== undefined) window.clearTimeout(timelineTimer);
      unsubscribe();
      attachmentsRef.current.forEach((attachment) => { if (attachment.preview) URL.revokeObjectURL(attachment.preview); });
    };
  }, [apiSession?.sourceHost.id, apiSession?.id, apiSession?.live, apiSession?.v2?.recordId, apiSession?.v2?.attachmentRevision]);

  useEffect(() => {
    const previousLength = previousDisplayLengthRef.current;
    previousDisplayLengthRef.current = displayItems.length;
    setTimelineWindowStart((current) => {
      if (loadingOlderRef.current) {
        loadingOlderRef.current = false;
        return current;
      }
      if (preserveScrollRef.current) return Math.min(current, Math.max(0, displayItems.length - 1));
      const followedLatest = current + MAX_TIMELINE_DOM_ITEMS >= previousLength;
      return followedLatest ? Math.max(0, displayItems.length - MAX_TIMELINE_DOM_ITEMS) : current;
    });
  }, [displayItems.length]);

  useEffect(() => {
    const viewport = scrollRef.current;
    if (skipAutoScrollRef.current) {
      skipAutoScrollRef.current = false;
      return;
    }
    if (viewport && !loading && !preserveScrollRef.current) viewport.scrollTop = viewport.scrollHeight;
  }, [loading, events, pending]);

  useLayoutEffect(() => {
    const pendingAnchor = anchorRestoreRef.current;
    const viewport = scrollRef.current;
    if (!pendingAnchor || !viewport) return;
    const anchor = viewport.querySelector<HTMLElement>(`[data-seq="${pendingAnchor.seq}"]`);
    if (anchor) viewport.scrollTop += anchor.getBoundingClientRect().top - pendingAnchor.top;
    anchorRestoreRef.current = null;
    preserveScrollRef.current = false;
  }, [renderedDisplayItems]);

  const loadEarlier = async () => {
    const currentSession = sessionRef.current;
    const viewport = scrollRef.current;
    const cursor = nextBeforeSeq ?? events[0]?.seq;
    if (!currentSession || !viewport || loadingEarlier) return;
    if (timelineWindowStart === 0 && (!hasMoreBefore || !cursor)) return;
    const anchor = viewport.querySelector<HTMLElement>('[data-seq]');
    const anchorSeq = anchor?.dataset.seq;
    const anchorTop = anchor?.getBoundingClientRect().top;
    preserveScrollRef.current = true;
    anchorRestoreRef.current = anchorSeq && anchorTop !== undefined ? { seq: anchorSeq, top: anchorTop } : null;
    if (timelineWindowStart > 0) {
      setTimelineWindowStart((current) => Math.max(0, current - TIMELINE_WINDOW_STEP));
      return;
    }
    setLoadingEarlier(true);
    skipAutoScrollRef.current = true;
    loadingOlderRef.current = true;
    try {
      const page = await loadTimelineBefore(currentSession, cursor);
      setEvents(page.events);
      setHasMoreBefore(page.hasMoreBefore);
      setNextBeforeSeq(page.nextBeforeSeq);
    } catch (nextError) {
      loadingOlderRef.current = false;
      skipAutoScrollRef.current = false;
      setError(nextError instanceof Error ? nextError.message : String(nextError));
      anchorRestoreRef.current = null;
      preserveScrollRef.current = false;
    } finally {
      setLoadingEarlier(false);
    }
  };

  const loadEarlierRef = useRef(loadEarlier);
  loadEarlierRef.current = loadEarlier;
  useEffect(() => {
    const viewport = scrollRef.current;
    if (!viewport) return;
    let frame = 0;
    const arm = () => { historyScrollArmedRef.current = true; };
    const onScroll = () => {
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        if (viewport.scrollTop > viewport.clientHeight * 2) historyScrollArmedRef.current = false;
        if (historyScrollArmedRef.current && viewport.scrollTop <= viewport.clientHeight) void loadEarlierRef.current();
      });
    };
    viewport.addEventListener('wheel', arm, { passive: true });
    viewport.addEventListener('scroll', onScroll, { passive: true });
    return () => {
      if (frame) cancelAnimationFrame(frame);
      viewport.removeEventListener('wheel', arm);
      viewport.removeEventListener('scroll', onScroll);
    };
  }, [session?.sourceHost.id, session?.id]);

  const addFiles = (files: File[]) => {
    const valid = files.filter((file) => file.size <= 20 * 1024 * 1024);
    if (valid.length !== files.length) setSendError('附件不能超过 20MB');
    if (valid.length > 0) {
      try {
        const next = valid.map((file) => {
          let preview: string | undefined;
          if (file.type.startsWith('image/')) {
            try { preview = URL.createObjectURL(file); } catch {
              setSendError('图片无法预览，仍可发送');
            }
          }
          return { id: attachmentId(), clientNonce: attachmentId(), file, preview, status: 'pending' as const, progress: 0 };
        });
        setAttachments((current) => [...current, ...next]);
      } catch (nextError) {
        setSendError(`添加附件失败：${nextError instanceof Error ? nextError.message : String(nextError)}`);
      }
    }
    setAttachmentMenuOpen(false);
  };

  const removeAttachment = (id: string) => {
    setAttachments((current) => {
      const removed = current.find((attachment) => attachment.id === id);
      if (removed?.preview) URL.revokeObjectURL(removed.preview);
      return current.filter((attachment) => attachment.id !== id);
    });
  };

  const uploadOne = async (attachment: PendingAttachment): Promise<string> => {
    if (!session) throw new Error('找不到会话');
    if (attachment.path) return attachment.path;
    setAttachments((current) => current.map((item) => item.id === attachment.id
      ? { ...item, status: 'uploading', progress: 0, error: undefined }
      : item));
    try {
      const path = await uploadSessionFile(session, attachment.file, (progress) => {
        setAttachments((current) => current.map((item) => item.id === attachment.id ? { ...item, progress } : item));
      }, attachment.clientNonce);
      setAttachments((current) => current.map((item) => item.id === attachment.id
        ? { ...item, status: 'uploaded', progress: 100, path }
        : item));
      return path;
    } catch (nextError) {
      const message = nextError instanceof Error ? nextError.message : String(nextError);
      setAttachments((current) => current.map((item) => item.id === attachment.id
        ? { ...item, status: 'error', error: message }
        : item));
      throw nextError;
    }
  };

  const retryAttachment = async (id: string) => {
    const attachment = attachmentsRef.current.find((item) => item.id === id);
    if (!attachment) return;
    setSendError(null);
    try { await uploadOne(attachment); } catch (nextError) {
      setSendError(nextError instanceof Error ? nextError.message : String(nextError));
    }
  };

  const send = async () => {
    const text = input.trim();
    const files = [...attachments];
    if (!session || !writable || (!text && files.length === 0) || sending) return;
    setSending(true);
    setSendError(null);
    let optimisticId = '';
    try {
      const paths = [];
      for (const attachment of files) paths.push(await uploadOne(attachment));
      const message = [text, ...paths].filter(Boolean).join('\n');
      const previousNonce = sendNonceRef.current;
      optimisticId = session.v2
        ? previousNonce?.text === message ? previousNonce.clientNonce : attachmentId()
        : attachmentId();
      if (session.v2) sendNonceRef.current = { text: message, clientNonce: optimisticId };
      const optimisticStatus = session.v2 ? 'submitting' as const : 'sending' as const;
      setPending((messages) => messages.some((item) => item.id === optimisticId)
        ? messages.map((item) => item.id === optimisticId ? { ...item, status: optimisticStatus } : item)
        : [...messages, { id: optimisticId, text: message, ts: new Date().toISOString(), status: optimisticStatus }]);
      const accepted = await sendSessionMessage(session, message, optimisticId);
      setPending((messages) => messages.map((item) => {
        if (item.id !== optimisticId) return item;
        if (session.v2 && accepted) {
          if (DELIVERY_RANK[item.status] > DELIVERY_RANK.accepted) return item;
          return { ...item, deliveryId: accepted.deliveryId, status: 'accepted' };
        }
        return { ...item, status: 'sent' };
      }));
      updateSessionState(session.sourceHost.id, session.id, { state: 'running' });
      setInput('');
      sendNonceRef.current = null;
      files.forEach((attachment) => { if (attachment.preview) URL.revokeObjectURL(attachment.preview); });
      setAttachments([]);
    } catch (nextError) {
      setPending((messages) => session.v2 && optimisticId
        ? messages.map((message) => message.id === optimisticId ? { ...message, status: 'failed' } : message)
        : messages.filter((message) => message.status !== 'sending'));
      if (nextError instanceof SessionSendError && nextError.status === 409) {
        updateSessionState(session.sourceHost.id, session.id, { canSend: false });
      } else {
        const reason = nextError instanceof Error ? nextError.message : String(nextError);
        setSendError(`发送失败：${reason}`);
      }
    } finally {
      setSending(false);
    }
  };

  const recover = async () => {
    setRecovering(true);
    try { await refreshSessions(); } finally { setRecovering(false); }
  };

  const shellClass = embedded ? 'timeline-shell embedded-timeline' : `app-shell timeline-shell${rcapp ? ' rcapp-shell' : ''}`;
  if (!session && (sessionsLoading || lastFetched === 0)) return <main className={shellClass} ref={shellRef}><div className="wa-empty">正在读取会话…</div></main>;
  if (!session) return <main className={shellClass} ref={shellRef}><div className="wa-empty error">找不到会话</div></main>;
  const suspectAttachment = session.v2?.attachmentStatus === 'suspect' || localSuspect;

  return (
    <main className={shellClass} ref={shellRef}>
      <header className={`wa-header timeline-header${rcapp ? ' rcapp-header' : ''}`}>
        {!embedded && (
          <button
            className="wa-header-btn"
            type="button"
            onClick={() => navigate({ pathname: '/', search: location.search })}
            aria-label="返回会话列表"
          >‹</button>
        )}
        <span className={`timeline-avatar kind-${session.kind}`}>{session.kind === 'claude' ? 'C' : 'X'}</span>
        <span className="timeline-heading">
          <span className="timeline-title">{session.title}</span>
          <span className="timeline-subtitle">
            <span className={`stream-dot ${streamState}`} />
            {session.sourceHost.name} · {session.kind} · <span data-qa="timeline-state-label">{SESSION_STATE_LABEL[session.state]}</span> · {session.model}
          </span>
        </span>
        {!composerAvailable && <span className="timeline-read-only" data-qa="timeline-read-only">{expired ? '过期' : !session.live ? '离线' : '只读'}</span>}
        <button
          className="timeline-kill"
          data-qa="timeline-kill"
          type="button"
          disabled={!writable}
          onClick={() => setKillOpen(true)}
          aria-label={`彻底关闭 ${session.title}`}
          title="彻底关闭 CLI"
        >
          <svg aria-hidden="true" viewBox="0 0 24 24">
            <path d="M12 2v10M6.34 5.34a8 8 0 1 0 11.32 0" />
          </svg>
        </button>
        {embedded && onClose && (
          <button className="timeline-close" data-qa="pane-close" type="button" onClick={onClose} aria-label={`关闭 ${session.title}`}>×</button>
        )}
      </header>

      {suspectAttachment && (
        <details className="v2-attachment-warning" data-qa="v2-attachment-warning">
          <summary>附着待确认：对话可能不符；消息仍会发送到当前终端入口</summary>
        </details>
      )}

      {terminalAvailable && (
        <div className="timeline-view-switch" data-qa="timeline-view-switch" role="group" aria-label="会话内容视图">
          <button type="button" data-qa="show-timeline" aria-pressed={contentView === 'timeline'} onClick={() => setContentView('timeline')}>时间线</button>
          <button type="button" data-qa="show-terminal-screen" aria-pressed={contentView === 'terminal'} onClick={() => setContentView('terminal')}>终端画面</button>
        </div>
      )}

      {contentView === 'terminal' && terminalAvailable ? (
        <TerminalScreen hostId={apiSession.sourceHost.id} entryId={apiSession.v2!.entryId!} enabled={writable} />
      ) : <div
        className="timeline-events"
        ref={scrollRef}
        data-qa="timeline"
        onPointerDown={() => { historyScrollArmedRef.current = true; }}
      >
        {!expired && events.length > 0 && (timelineWindowStart > 0 || hasMoreBefore ? (
          <button className="timeline-load-earlier" data-qa="timeline-load-earlier" type="button" disabled={loadingEarlier} onClick={() => void loadEarlier()}>
            {loadingEarlier ? '正在加载更早消息…' : timelineWindowStart > 0 ? '显示更早消息' : '加载更早消息'}
          </button>
        ) : <div className="timeline-start" data-qa="timeline-start">已到会话开头</div>)}
        {!expired && cacheRefreshing && events.length > 0 && <div className="timeline-cache-updating" data-qa="timeline-cache-updating">已显示本地记录 · 更新中…</div>}
        {expired && <div className="expired-record" data-qa="expired-record">记录已过期</div>}
        {!expired && streamState === 'reconnecting' && <div className="wa-empty">重新连接中…</div>}
        {!expired && loading && events.length === 0 && streamState !== 'reconnecting' && <div className="wa-empty">正在读取最新消息…</div>}
        {!expired && error && <div className="wa-empty error">{error}</div>}
        {!expired && !loading && !error && items.length === 0 && <div className="wa-empty">暂无时间线事件</div>}
        {!expired && renderedDisplayItems.map((entry) => entry.type === 'event' ? (
          <Event
            key={`${'kind' in entry.item ? 'tool' : entry.item.type}:${entry.item.seq}`}
            item={entry.item}
            canChoose={writable}
            onChoose={chooseTimelineOption}
          />
        ) : (
          <div
            className={`message-row user pending status-${entry.message.status}`}
            data-qa={entry.message.status === 'sending' || entry.message.status === 'submitting' ? 'sending-user-bubble' : 'pending-user-bubble'}
            data-delivery-status={entry.message.status}
            key={entry.message.id}
          >
            <div className="message-bubble"><UserMessageContent text={entry.message.text} /><time>{time(entry.message.ts)} · {deliveryLabel(entry.message.status)}</time></div>
          </div>
        ))}
        {!expired && timelineWindowStart + MAX_TIMELINE_DOM_ITEMS < displayItems.length && (
          <button
            className="timeline-load-earlier timeline-show-newer"
            data-qa="timeline-show-newer"
            type="button"
            onClick={() => setTimelineWindowStart((current) => Math.min(displayItems.length - MAX_TIMELINE_DOM_ITEMS, current + TIMELINE_WINDOW_STEP))}
          >显示更新消息</button>
        )}
        {session.state === 'running' || session.state === 'waiting_input' ? (
          <div className={`timeline-work-state state-${session.state}`} data-qa="timeline-footer-state" data-state={session.state}>
            <span className="work-state-dot" aria-hidden="true" />
            <span data-qa="timeline-event-age" data-anchor-ts={latestEventAt}>
              {session.state === 'running' ? '工作中' : '等待输入'} · {formatEventAge(latestEventAt, ageNow)}
            </span>
          </div>
        ) : (
          <div className="timeline-event-age" data-qa="timeline-event-age" data-footer-qa="timeline-footer-state" data-anchor-ts={latestEventAt}>
            {formatEventAge(latestEventAt, ageNow)}
          </div>
        )}
      </div>}

      {composerAvailable ? (
        <div className="timeline-composer">
          {!writable && <div className="composer-connecting" data-qa="composer-connecting">正在连接，输入暂不可用</div>}
          {sendError && <div className="send-error">{sendError}</div>}
          {attachments.length > 0 && (
            <div className="attachment-previews" data-qa="attachment-previews">
              {attachments.map((attachment) => (
                <div className={`attachment-preview status-${attachment.status}`} data-qa="attachment-preview" key={attachment.id}>
                  {attachment.preview ? (
                    <img
                      src={attachment.preview}
                      alt=""
                      onError={() => {
                        URL.revokeObjectURL(attachment.preview!);
                        setAttachments((current) => current.map((item) => item.id === attachment.id ? { ...item, preview: undefined } : item));
                        setSendError('图片无法预览，仍可发送');
                      }}
                    />
                  ) : <span className="attachment-file-icon" aria-hidden="true">📎</span>}
                  <span className="attachment-copy">
                    <span>{attachment.file.name}</span>
                    <small>{attachment.status === 'uploading' ? `上传中 ${attachment.progress}%` : attachment.status === 'uploaded' ? '已上传' : attachment.status === 'error' ? attachment.error : `${Math.max(1, Math.round(attachment.file.size / 1024))} KB`}</small>
                  </span>
                  {attachment.status === 'error' && <button data-qa="retry-upload" type="button" onClick={() => retryAttachment(attachment.id)}>重试</button>}
                  <button className="attachment-remove" data-qa="remove-attachment" type="button" disabled={attachment.status === 'uploading'} onClick={() => removeAttachment(attachment.id)} aria-label={`移除 ${attachment.file.name}`}>×</button>
                  {attachment.status === 'uploading' && <span className="attachment-progress" style={{ width: `${attachment.progress}%` }} />}
                </div>
              ))}
            </div>
          )}
          <div className="composer-row">
            <div className="attachment-picker">
              <button className="composer-add" data-qa="attachment-add" type="button" disabled={!writable || sending} onClick={() => setAttachmentMenuOpen((value) => !value)} aria-label="添加附件">＋</button>
              {attachmentMenuOpen && (
                <div className="attachment-menu" data-qa="attachment-menu">
                  <button type="button" onClick={() => cameraInputRef.current?.click()}>拍照</button>
                  <button type="button" onClick={() => galleryInputRef.current?.click()}>相册</button>
                  <button type="button" onClick={() => fileInputRef.current?.click()}>文件</button>
                </div>
              )}
              <input ref={cameraInputRef} data-qa="camera-input" hidden type="file" accept="image/*" capture="environment" onChange={(event) => { addFiles([...event.currentTarget.files ?? []]); event.currentTarget.value = ''; }} />
              <input ref={galleryInputRef} data-qa="gallery-input" hidden type="file" accept="image/*" multiple onChange={(event) => { addFiles([...event.currentTarget.files ?? []]); event.currentTarget.value = ''; }} />
              <input ref={fileInputRef} data-qa="file-input" hidden type="file" accept="*/*" multiple onChange={(event) => { addFiles([...event.currentTarget.files ?? []]); event.currentTarget.value = ''; }} />
            </div>
            <textarea
              data-qa="chat-input"
              rows={1}
              value={input}
              disabled={!writable}
              onChange={(event) => setInput(event.target.value)}
              onPaste={(event) => {
                const images = [...event.clipboardData.items]
                  .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
                  .map((item) => item.getAsFile())
                  .filter((file): file is File => file !== null);
                if (images.length > 0) {
                  event.preventDefault();
                  addFiles(images);
                }
              }}
              onKeyDown={(event) => {
                const enter = event.key === 'Enter' || event.key === 'NumpadEnter' || event.code === 'NumpadEnter';
                if (enter && !event.shiftKey) {
                  event.preventDefault();
                  send();
                }
              }}
              placeholder={writable ? '输入消息…' : '正在连接…'}
              aria-label="输入消息"
            />
            <button data-qa="chat-send" type="button" onClick={send} disabled={!writable || (!input.trim() && attachments.length === 0) || sending} aria-label="发送">
              {sending ? '…' : '➤'}
            </button>
          </div>
        </div>
      ) : (
        <div className="read-only-banner" data-qa="read-only">
          <button className="read-only-add" data-qa="attachment-add" type="button" disabled aria-label="添加附件不可用">＋</button>
          <span>{expired
            ? '记录已过期'
            : !session.live
              ? '离线 · 只读历史'
              : session.v2?.target === 'entry' && session.v2.transportState === 'unreachable'
                  ? '主机不可达 · 只读，写入暂时禁用'
                  : '🔒 只读（未绑定到终端）'}</span>
          <button className="recover-connection" data-qa="recover-connection" type="button" disabled={recovering} onClick={recover}>
            {recovering ? '正在恢复…' : '尝试恢复连接'}
          </button>
          {sessionsError && <small className="recover-error">{sessionsError}</small>}
        </div>
      )}
      <KillSessionDialog
        session={session}
        open={killOpen}
        onClose={() => setKillOpen(false)}
        onKilled={() => embedded ? onClose?.() : navigate({ pathname: '/', search: location.search })}
      />
    </main>
  );
}
