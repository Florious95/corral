import { useEffect, useRef, useState } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import type { TimelineEvent } from '../api';
import { useDesktopLayout } from '../hooks/useDesktopLayout';
import { ReadOnlyTimelineEvents, UserMessageContent } from '../pages/SessionTimeline';
import { migrateV1Favorites, toggleV2Favorite, useV2Favorites } from './favorites';
import {
  chooseV2EntryOption,
  getV2EntryTimeline,
  getV2RecordTimeline,
  killV2Entry,
  refreshV2Host,
  sendV2EntryMessage,
  subscribeV2EntryStream,
  uploadV2EntryFile,
  useV2LiveState,
  V2EntryWriteError,
} from './liveData';
import type { V2DeliveryStatus } from './types';
import { EntryGroups, FavoriteGroup, HistoryGroups, HostDot } from './V2MockConsole';

type PendingDeliveryStatus = 'submitting' | V2DeliveryStatus | 'failed';

interface PendingDelivery {
  clientNonce: string;
  deliveryId?: string;
  text: string;
  ts: string;
  status: PendingDeliveryStatus;
}

interface PendingAttachment {
  id: string;
  clientNonce: string;
  file: File;
  preview?: string;
  path?: string;
  progress: number;
  status: 'pending' | 'uploading' | 'uploaded' | 'error';
  error?: string;
}

function nonce(): string {
  return typeof crypto.randomUUID === 'function'
    ? crypto.randomUUID()
    : `v2-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function mergeEvents(current: TimelineEvent[], incoming: TimelineEvent[]): TimelineEvent[] {
  const bySeq = new Map(current.map((event) => [event.seq, event]));
  incoming.forEach((event) => bySeq.set(event.seq, event));
  return [...bySeq.values()].sort((left, right) => left.seq - right.seq);
}

function comparableMessageText(text: string): string {
  return text
    .replace(/\r\n?/g, '\n')
    .split('\n')
    .map((line) => {
      const trimmed = line.trim();
      const path = trimmed.startsWith('@/') ? trimmed.slice(1) : trimmed;
      return path.startsWith('/') && path.includes('/corral-uploads/') ? path : line;
    })
    .join('\n')
    .trim();
}

const DELIVERY_RANK: Record<PendingDeliveryStatus, number> = {
  submitting: 0,
  failed: 0,
  accepted: 1,
  echoed: 2,
  unattributed: 2,
};

function deliveryLabel(status: PendingDeliveryStatus): string {
  if (status === 'accepted') return '发送中';
  if (status === 'echoed') return '已送达';
  if (status === 'unattributed') return '已发送 · 附着待确认';
  if (status === 'failed') return '发送失败';
  return '正在提交…';
}

export function V2LiveConsole() {
  const location = useLocation();
  const desktop = useDesktopLayout();
  const view = useV2LiveState();
  const favorites = useV2Favorites();
  useEffect(() => migrateV1Favorites(view.snapshots), [view.snapshots]);

  return (
    <main className="v2-console" data-qa="v2-console" data-mode="live-write">
      <header className="wa-header v2-header">
        <span><strong>AI 终端</strong><small>窗格优先 v2 · D 联调</small></span>
        <span className="data-mode live">V2 ENTRY</span>
      </header>
      <div className="v2-host-status-slot">
        {view.failedHosts.length > 0 && <div className="v2-snapshot-notice" data-qa="v2-host-warning">部分主机暂不可达：{view.failedHosts.map((host) => host.name).join('、')}</div>}
      </div>
      <div className="v2-list-layout">
        <div className="v2-list-scroll">
          <FavoriteGroup entries={view.entries} history={view.history} favorites={favorites} search={location.search} onToggleFavorite={toggleV2Favorite} />
          <section className="v2-zone v2-active-zone" data-qa="v2-active-zone">
            <h1>活跃终端 <small>{view.entries.length}</small></h1>
            <EntryGroups entries={view.entries} search={location.search} favorites={favorites} onToggleFavorite={toggleV2Favorite} />
            {view.loadingHosts.length > 0 && <div className="v2-loading-hosts" data-qa="v2-loading-hosts">正在读取：{view.loadingHosts.map((host) => host.name).join('、')}</div>}
            {view.ready && view.entries.length === 0 && <div className="wa-empty">暂无活跃终端</div>}
          </section>
          <section className="v2-zone v2-history-zone" data-qa="v2-history-zone">
            <h1>历史记录 <small>{view.history.length}</small></h1>
            <HistoryGroups history={view.history} search={location.search} favorites={favorites} onToggleFavorite={toggleV2Favorite} />
            {view.ready && view.history.length === 0 && <div className="wa-empty">暂无历史记录</div>}
          </section>
          {view.error && view.snapshots.length === 0 && <div className="wa-empty error" data-qa="v2-load-error">{view.error}</div>}
        </div>
        {desktop && <aside className="v2-desktop-preview"><strong>终端入口写入</strong><p>消息、附件、选项与关闭都锚定 entryId；纯历史记录保持只读。</p></aside>}
      </div>
    </main>
  );
}

export function V2LiveTimeline({ kind }: { kind: 'entry' | 'record' }) {
  const { hostId = '', entryId = '', recordId = '' } = useParams();
  const location = useLocation();
  const navigate = useNavigate();
  const view = useV2LiveState();
  const hostedEntry = view.entries.find(({ host, entry }) => host.id === hostId && entry.entryId === entryId);
  const hostedRecord = view.history.find(({ host, record }) => host.id === hostId && record.recordId === recordId);
  const loadingHost = view.loadingHosts.some((host) => host.id === hostId);
  const [events, setEvents] = useState<TimelineEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [input, setInput] = useState('');
  const [sending, setSending] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  const [pending, setPending] = useState<PendingDelivery[]>([]);
  const [lastDelivery, setLastDelivery] = useState<{ clientNonce: string; status: PendingDeliveryStatus } | null>(null);
  const [attachments, setAttachments] = useState<PendingAttachment[]>([]);
  const [attachmentMenuOpen, setAttachmentMenuOpen] = useState(false);
  const [localSuspect, setLocalSuspect] = useState(false);
  const [entryStreamReady, setEntryStreamReady] = useState(false);
  const [goneReason, setGoneReason] = useState<string | null>(null);
  const [killOpen, setKillOpen] = useState(false);
  const [killing, setKilling] = useState(false);
  const scrollRef = useRef<HTMLElement>(null);
  const cameraInputRef = useRef<HTMLInputElement>(null);
  const galleryInputRef = useRef<HTMLInputElement>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  const latestSeqRef = useRef(0);
  const attachmentsRef = useRef(attachments);
  const sendNonceRef = useRef<{ text: string; clientNonce: string } | null>(null);
  const chooseNoncesRef = useRef(new Map<string, string>());
  const killNonceRef = useRef(nonce());
  attachmentsRef.current = attachments;

  const routeAvailable = kind === 'entry' ? Boolean(hostedEntry) : Boolean(hostedRecord);
  const entryRevision = hostedEntry?.entry.attachmentRevision;
  useEffect(() => {
    if (routeAvailable) return;
    setLoading(loadingHost || !view.ready);
    setEvents([]);
  }, [routeAvailable, loadingHost, view.ready]);

  useEffect(() => {
    if (!routeAvailable) return;
    let alive = true;
    let unsubscribe = () => {};
    if (kind === 'entry') setEntryStreamReady(false);
    setLoading(true);
    setError(null);
    setGoneReason(null);
    latestSeqRef.current = 0;
    const request = kind === 'entry'
      ? getV2EntryTimeline(hostId, entryId).then((timeline) => timeline.events)
      : getV2RecordTimeline(hostId, recordId);
    request
      .then((next) => {
        if (!alive) return;
        latestSeqRef.current = next.at(-1)?.seq ?? 0;
        setEvents(next);
        if (kind !== 'entry') return;
        unsubscribe = subscribeV2EntryStream(hostId, entryId, latestSeqRef.current, {
          onOpen: () => { if (alive) setEntryStreamReady(true); },
          onTimeline: (event) => {
            if (!alive) return;
            latestSeqRef.current = Math.max(latestSeqRef.current, event.seq);
            setEvents((current) => mergeEvents(current, [event]));
          },
          onState: () => { void refreshV2Host(hostId).catch(() => null); },
          onDelivery: (delivery) => {
            if (!alive) return;
            setPending((current) => current.map((message) => {
              if (message.clientNonce !== delivery.clientNonce) return message;
              if (message.deliveryId && message.deliveryId !== delivery.deliveryId) return message;
              if (DELIVERY_RANK[delivery.status] < DELIVERY_RANK[message.status]) return message;
              return { ...message, deliveryId: delivery.deliveryId, status: delivery.status };
            }));
            setLastDelivery((current) => {
              if (!current || current.clientNonce !== delivery.clientNonce || DELIVERY_RANK[current.status] > DELIVERY_RANK[delivery.status]) return current;
              return { clientNonce: delivery.clientNonce, status: delivery.status };
            });
            if (delivery.status === 'unattributed') {
              setLocalSuspect(true);
              void refreshV2Host(hostId).catch(() => null);
            }
          },
          onAttachmentChanged: (nextRevision) => {
            if (nextRevision > (entryRevision ?? -1)) void refreshV2Host(hostId).catch(() => null);
          },
          onEntryRemoved: () => {
            setGoneReason('终端入口已失效');
            void refreshV2Host(hostId).catch(() => null);
            navigate({ pathname: '/', search: location.search });
          },
          onSnapshotRequired: () => { void refreshV2Host(hostId).catch(() => null); },
          onError: () => {
            if (!alive) return;
            setEntryStreamReady(false);
            getV2EntryTimeline(hostId, entryId)
              .then((timeline) => {
                if (!alive) return;
                const missing = timeline.events.filter((event) => event.seq > latestSeqRef.current);
                latestSeqRef.current = Math.max(latestSeqRef.current, missing.at(-1)?.seq ?? 0);
                setEvents((current) => mergeEvents(current, missing));
              })
              .catch(() => {});
          },
        });
      })
      .catch((nextError) => { if (alive) setError(nextError instanceof Error ? nextError.message : String(nextError)); })
      .finally(() => { if (alive) setLoading(false); });
    return () => {
      alive = false;
      unsubscribe();
    };
  }, [kind, hostId, entryId, recordId, entryRevision, routeAvailable, location.search, navigate]);

  useEffect(() => {
    setInput('');
    setSendError(null);
    setPending([]);
    setLastDelivery(null);
    setLocalSuspect(false);
    setAttachmentMenuOpen(false);
    sendNonceRef.current = null;
    chooseNoncesRef.current.clear();
    killNonceRef.current = nonce();
    attachmentsRef.current.forEach((attachment) => { if (attachment.preview) URL.revokeObjectURL(attachment.preview); });
    setAttachments([]);
    return () => {
      attachmentsRef.current.forEach((attachment) => { if (attachment.preview) URL.revokeObjectURL(attachment.preview); });
    };
  }, [hostId, entryId, recordId]);

  useEffect(() => {
    if (!events.some((event) => event.type === 'user_message')) return;
    setPending((current) => current.filter((message) => !events.some((event) => event.type === 'user_message' && comparableMessageText(event.text) === comparableMessageText(message.text))));
  }, [events]);

  useEffect(() => {
    if (!loading && scrollRef.current) scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
  }, [events, loading, pending]);

  const handleWriteError = async (nextError: unknown): Promise<string> => {
    if (nextError instanceof V2EntryWriteError && nextError.status === 410) {
      setGoneReason('入口已失效，正在刷新终端列表');
      await refreshV2Host(hostId).catch(() => null);
      return `入口已失效：${nextError.message}`;
    }
    return nextError instanceof Error ? nextError.message : String(nextError);
  };

  const addFiles = (files: File[]) => {
    const valid = files.filter((file) => file.size <= 20 * 1024 * 1024);
    if (valid.length !== files.length) setSendError('附件不能超过 20MB');
    const next = valid.map((file) => {
      let preview: string | undefined;
      if (file.type.startsWith('image/')) {
        try { preview = URL.createObjectURL(file); } catch {}
      }
      return {
        id: nonce(),
        clientNonce: nonce(),
        file,
        preview,
        progress: 0,
        status: 'pending' as const,
      };
    });
    if (next.length > 0) setAttachments((current) => [...current, ...next]);
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
    if (attachment.path) return attachment.path;
    setAttachments((current) => current.map((item) => item.id === attachment.id ? { ...item, status: 'uploading', error: undefined } : item));
    try {
      const result = await uploadV2EntryFile(hostId, entryId, attachment.file, attachment.clientNonce, (progress) => {
        setAttachments((current) => current.map((item) => item.id === attachment.id ? { ...item, progress } : item));
      });
      setAttachments((current) => current.map((item) => item.id === attachment.id ? { ...item, path: result.path, progress: 100, status: 'uploaded' } : item));
      return result.path;
    } catch (nextError) {
      const reason = await handleWriteError(nextError);
      setAttachments((current) => current.map((item) => item.id === attachment.id ? { ...item, status: 'error', error: reason } : item));
      throw nextError;
    }
  };

  const send = async () => {
    const caption = input.trim();
    const files = [...attachments];
    if (!hostedEntry || hostedEntry.transportState !== 'reachable' || sending || (!caption && files.length === 0)) return;
    setSending(true);
    setSendError(null);
    let optimisticNonce = '';
    try {
      const paths = [];
      for (const attachment of files) paths.push(await uploadOne(attachment));
      const message = [caption, ...paths].filter(Boolean).join('\n');
      const existing = sendNonceRef.current;
      optimisticNonce = existing?.text === message ? existing.clientNonce : nonce();
      sendNonceRef.current = { text: message, clientNonce: optimisticNonce };
      setPending((current) => current.some((item) => item.clientNonce === optimisticNonce)
        ? current.map((item) => item.clientNonce === optimisticNonce ? { ...item, status: 'submitting' } : item)
        : [...current, { clientNonce: optimisticNonce, text: message, ts: new Date().toISOString(), status: 'submitting' }]);
      setLastDelivery({ clientNonce: optimisticNonce, status: 'submitting' });
      const accepted = await sendV2EntryMessage(hostId, entryId, message, optimisticNonce);
      setPending((current) => current.map((item) => {
        if (item.clientNonce !== optimisticNonce || DELIVERY_RANK[item.status] > DELIVERY_RANK.accepted) return item;
        return { ...item, deliveryId: accepted.deliveryId, status: 'accepted' };
      }));
      setLastDelivery((current) => current?.clientNonce === optimisticNonce && DELIVERY_RANK[current.status] > DELIVERY_RANK.accepted
        ? current
        : { clientNonce: optimisticNonce, status: 'accepted' });
      setInput('');
      sendNonceRef.current = null;
      files.forEach((attachment) => { if (attachment.preview) URL.revokeObjectURL(attachment.preview); });
      setAttachments([]);
    } catch (nextError) {
      const reason = await handleWriteError(nextError);
      if (optimisticNonce) setPending((current) => current.map((item) => item.clientNonce === optimisticNonce ? { ...item, status: 'failed' } : item));
      if (optimisticNonce) setLastDelivery({ clientNonce: optimisticNonce, status: 'failed' });
      setSendError(`发送失败：${reason}`);
    } finally {
      setSending(false);
    }
  };

  const choose = async (option: number, seq: number) => {
    const key = `${seq}:${option}`;
    const clientNonce = chooseNoncesRef.current.get(key) ?? nonce();
    chooseNoncesRef.current.set(key, clientNonce);
    try {
      await chooseV2EntryOption(hostId, entryId, option, clientNonce);
    } catch (nextError) {
      const reason = await handleWriteError(nextError);
      throw new Error(reason);
    }
  };

  const kill = async () => {
    setKilling(true);
    setSendError(null);
    try {
      await killV2Entry(hostId, entryId, killNonceRef.current);
      await refreshV2Host(hostId).catch(() => null);
      navigate({ pathname: '/', search: location.search });
    } catch (nextError) {
      setSendError(`关闭失败：${await handleWriteError(nextError)}`);
      setKillOpen(false);
    } finally {
      setKilling(false);
    }
  };

  if (loading && !hostedEntry && !hostedRecord) return <main className="v2-timeline"><div className="wa-empty">正在读取 v2 时间线…</div></main>;
  if (kind === 'entry' && !hostedEntry) return <main className="v2-timeline"><div className="wa-empty error">{goneReason ?? '终端入口已消失'}</div></main>;
  if (kind === 'record' && !hostedRecord) return <main className="v2-timeline"><div className="wa-empty error">历史记录不存在</div></main>;

  const entry = hostedEntry?.entry;
  const record = hostedRecord?.record;
  const host = hostedEntry?.host ?? hostedRecord!.host;
  const title = entry?.attachment?.title ?? (entry ? '未识别对话' : record!.title);
  const transportReachable = hostedEntry?.transportState === 'reachable';
  const writable = Boolean(entry && transportReachable && entryStreamReady);
  const suspect = Boolean(entry && (entry.attachment?.status === 'suspect' || localSuspect));
  return (
    <main className="v2-timeline" data-qa="v2-timeline" data-mode="live-write" data-entry-id={entry?.entryId ?? ''} data-record-id={record?.recordId ?? entry?.attachment?.recordId ?? ''}>
      <header className="wa-header v2-timeline-header">
        <button type="button" onClick={() => navigate({ pathname: '/', search: location.search })} aria-label="返回终端列表">‹</button>
        <span><strong>{title}</strong><small><HostDot id={host.id} />{host.name} · {entry ? `${entry.kind} · ${entry.pane.paneId}` : '只读历史'}</small></span>
        <span className={entry ? 'v2-entry-badge' : 'v2-history-badge'}>{entry ? '终端入口' : '只读'}</span>
        {entry && <button className="v2-kill" data-qa="v2-kill" type="button" disabled={!writable} onClick={() => setKillOpen(true)} aria-label={`彻底关闭 ${title}`}>⏻</button>}
      </header>
      {suspect && (
        <details className="v2-attachment-warning" data-qa="v2-attachment-warning">
          <summary>附着待确认：对话可能不符；消息仍会发送到当前终端入口</summary>
          <p>原因：{entry?.attachment?.suspectReason ?? 'delivery_unattributed'} · {entry?.cwd} · {entry?.pane.paneId}</p>
        </details>
      )}
      <section className="timeline-events v2-timeline-body" ref={scrollRef} data-qa="v2-timeline-events">
        {loading && <div className="wa-empty">正在读取时间线…</div>}
        {error && <div className="wa-empty error">{error}</div>}
        {goneReason && <div className="wa-empty error" data-qa="v2-entry-gone">{goneReason}</div>}
        {!loading && !error && entry && !entry.attachment && <div className="v2-unidentified-empty" data-qa="v2-unidentified-empty"><strong>尚未识别对话</strong><span>当前没有可回放的记录。</span></div>}
        {!loading && !error && events.length === 0 && (record || entry?.attachment) && <div className="wa-empty">暂无时间线事件</div>}
        {!loading && !error && <ReadOnlyTimelineEvents events={events} canChoose={writable} onChoose={choose} />}
        {pending.map((message) => (
          <div className={`message-row user pending v2-delivery-${message.status}`} data-qa="v2-pending-bubble" data-delivery-status={message.status} data-client-nonce={message.clientNonce} key={message.clientNonce}>
            <div className="message-bubble"><UserMessageContent text={message.text} /><time>{deliveryLabel(message.status)}</time></div>
          </div>
        ))}
      </section>
      {entry ? (
        <footer className="timeline-composer v2-live-composer">
          {!writable && <div className="v2-transport-warning" data-qa="v2-write-disabled">{transportReachable ? '正在连接终端事件流，写入暂时禁用' : '主机不可达，写入暂时禁用'}</div>}
          {lastDelivery && <div className={`v2-delivery-state status-${lastDelivery.status}`} data-qa="v2-delivery-state" data-delivery-status={lastDelivery.status}>{deliveryLabel(lastDelivery.status)}</div>}
          {sendError && <div className="send-error" data-qa="v2-send-error">{sendError}</div>}
          {attachments.length > 0 && (
            <div className="attachment-previews" data-qa="v2-attachment-previews">
              {attachments.map((attachment) => (
                <div className={`attachment-preview status-${attachment.status}`} data-qa="v2-attachment-preview" key={attachment.id}>
                  {attachment.preview ? <img src={attachment.preview} alt="" /> : <span className="attachment-file-icon">📎</span>}
                  <span className="attachment-copy"><span>{attachment.file.name}</span><small>{attachment.status === 'uploading' ? `上传中 ${attachment.progress}%` : attachment.status === 'uploaded' ? '已上传' : attachment.error ?? `${Math.max(1, Math.round(attachment.file.size / 1024))} KB`}</small></span>
                  <button type="button" disabled={attachment.status === 'uploading'} onClick={() => removeAttachment(attachment.id)} aria-label={`移除 ${attachment.file.name}`}>×</button>
                </div>
              ))}
            </div>
          )}
          <div className="composer-row v2-composer-row">
            <div className="attachment-picker">
              <button className="composer-add" data-qa="v2-attachment-add" type="button" disabled={!writable || sending} onClick={() => setAttachmentMenuOpen((current) => !current)} aria-label="添加附件">＋</button>
              {attachmentMenuOpen && (
                <div className="attachment-menu" data-qa="v2-attachment-menu">
                  <button type="button" onClick={() => cameraInputRef.current?.click()}>拍照</button>
                  <button type="button" onClick={() => galleryInputRef.current?.click()}>相册</button>
                  <button type="button" onClick={() => fileInputRef.current?.click()}>文件</button>
                </div>
              )}
              <input ref={cameraInputRef} data-qa="v2-camera-input" hidden type="file" accept="image/*" capture="environment" onChange={(event) => { addFiles([...event.currentTarget.files ?? []]); event.currentTarget.value = ''; }} />
              <input ref={galleryInputRef} data-qa="v2-gallery-input" hidden type="file" accept="image/*" multiple onChange={(event) => { addFiles([...event.currentTarget.files ?? []]); event.currentTarget.value = ''; }} />
              <input ref={fileInputRef} data-qa="v2-file-input" hidden type="file" accept="*/*" multiple onChange={(event) => { addFiles([...event.currentTarget.files ?? []]); event.currentTarget.value = ''; }} />
            </div>
            <textarea
              data-qa="v2-chat-input"
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
                  void send();
                }
              }}
              placeholder="发消息到这个终端入口"
              aria-label="发消息到这个终端入口"
            />
            <button data-qa="v2-send" type="button" disabled={!writable || sending || (!input.trim() && attachments.length === 0)} onClick={() => void send()}>{sending ? '…' : '发送'}</button>
          </div>
        </footer>
      ) : <footer className="v2-history-readonly">历史记录只读</footer>}
      {killOpen && entry && (
        <div className="danger-dialog-backdrop" data-qa="v2-kill-dialog" role="presentation">
          <section className="danger-dialog" role="dialog" aria-modal="true" aria-labelledby="v2-kill-title">
            <div className="danger-dialog-mark" aria-hidden="true">!</div>
            <h2 id="v2-kill-title">彻底关闭 CLI？</h2>
            <dl><dt>会话</dt><dd>{title}</dd><dt>目录</dt><dd>{entry.cwd}</dd><dt>主机</dt><dd>{host.name}</dd></dl>
            <p>将终止该 CLI 进程及其 tmux 窗格，不可恢复。</p>
            <div className="danger-dialog-actions"><button type="button" disabled={killing} onClick={() => setKillOpen(false)}>取消</button><button className="danger-confirm" data-qa="v2-kill-confirm" type="button" disabled={killing} onClick={() => void kill()}>{killing ? '正在关闭…' : '彻底关闭'}</button></div>
          </section>
        </div>
      )}
    </main>
  );
}
