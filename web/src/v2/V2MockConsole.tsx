import { useMemo, useState } from 'react';
import { Link, useLocation, useNavigate, useParams } from 'react-router-dom';
import { hostIdentityColor } from '../favorites';
import { useDesktopLayout } from '../hooks/useDesktopLayout';
import { GROUP_PREVIEW_LIMIT } from '../sessionGroups';
import { v2RecordIdentity } from './favorites';
import { applyAttachment, v2MockView, type HostedV2Entry, type HostedV2History } from './mockModel';
import type { V2EntryState } from './types';

const STATE_LABEL: Record<V2EntryState, string> = {
  running: '运行中',
  waiting_input: '等待输入',
  idle: '空闲',
  unknown: '状态未知',
};

function cwdLabel(cwd: string): string {
  const parts = cwd.split('/').filter(Boolean);
  return parts.slice(-2).join(' / ') || cwd;
}

function useScenario(): string {
  return new URLSearchParams(useLocation().search).get('scenario') ?? 'base';
}

function entryPath(hostId: string, entryId: string, search: string): string {
  return `/v2/entries/${encodeURIComponent(hostId)}/${encodeURIComponent(entryId)}${search}`;
}

function recordPath(hostId: string, recordId: string, search: string): string {
  return `/v2/records/${encodeURIComponent(hostId)}/${encodeURIComponent(recordId)}${search}`;
}

export function HostDot({ id }: { id: string }) {
  return <span className="host-identity-dot" data-host-id={id} style={{ backgroundColor: hostIdentityColor(id) }} />;
}

export function EntryItem({ hosted, search, favorite = false, onToggleFavorite }: {
  hosted: HostedV2Entry;
  search: string;
  favorite?: boolean;
  onToggleFavorite?: (hostId: string, recordId: string) => void;
}) {
  const { entry, host, transportState } = hosted;
  const title = entry.attachment?.title ?? '未识别对话';
  const recordId = entry.attachment?.recordId;
  return (
    <div
      className={`v2-item-row v2-entry state-${entry.state}${transportState !== 'reachable' ? ' transport-disabled' : ''}`}
      data-qa="v2-entry"
      data-entry-id={entry.entryId}
      data-record-id={recordId ?? ''}
      data-attachment-status={entry.attachment?.status ?? 'unidentified'}
      data-transport-state={transportState}
    >
      <Link className="v2-item" to={entryPath(host.id, entry.entryId, search)}>
        <span className="v2-kind">{entry.kind === 'claude' ? 'C' : 'X'}</span>
        <span className="v2-item-copy">
          <span className="v2-item-title">{favorite && !onToggleFavorite && '★ '}{title}</span>
          <span className="v2-item-meta">
            <span className={`state-dot state-${entry.state === 'unknown' ? 'gone' : entry.state}`} />
            {STATE_LABEL[entry.state]} · {entry.pane.paneId} · {entry.model || '模型未知'}
          </span>
          <span className="v2-item-preview">{entry.lastMessagePreview || cwdLabel(entry.cwd)}</span>
        </span>
        <span className="v2-item-badges">
          <span className="v2-entry-badge">终端入口</span>
          {!entry.attachment && <span className="v2-unidentified" data-qa="v2-unidentified">未识别</span>}
          {entry.attachment?.status === 'suspect' && <span className="v2-suspect" data-qa="v2-suspect">对话待确认</span>}
          {transportState !== 'reachable' && <span className="v2-transport" data-qa="v2-transport">主机状态未知</span>}
        </span>
      </Link>
      {recordId && onToggleFavorite && (
        <button className={`v2-favorite-toggle${favorite ? ' active' : ''}`} data-qa="v2-favorite-toggle" type="button" onClick={() => onToggleFavorite(host.id, recordId)} aria-label={favorite ? `取消收藏 ${title}` : `收藏 ${title}`}>★</button>
      )}
    </div>
  );
}

export function HistoryItem({ hosted, search, favorite = false, onToggleFavorite }: {
  hosted: HostedV2History;
  search: string;
  favorite?: boolean;
  onToggleFavorite?: (hostId: string, recordId: string) => void;
}) {
  const { host, record } = hosted;
  return (
    <div
      className="v2-item-row v2-history-item"
      data-qa="v2-history"
      data-record-id={record.recordId}
    >
      <Link className="v2-item" to={recordPath(host.id, record.recordId, search)}>
        <span className="v2-kind">{record.kind === 'claude' ? 'C' : 'X'}</span>
        <span className="v2-item-copy">
          <span className="v2-item-title">{favorite && !onToggleFavorite && '★ '}{record.title}</span>
          <span className="v2-item-meta">{record.model} · {host.name}</span>
          <span className="v2-item-preview">{record.preview}</span>
        </span>
        <span className="v2-item-badges"><span className="v2-history-badge">只读历史</span></span>
      </Link>
      {onToggleFavorite && <button className={`v2-favorite-toggle${favorite ? ' active' : ''}`} data-qa="v2-favorite-toggle" type="button" onClick={() => onToggleFavorite(host.id, record.recordId)} aria-label={favorite ? `取消收藏 ${record.title}` : `收藏 ${record.title}`}>★</button>}
    </div>
  );
}

function GroupHeader({ title, hostId, count }: { title: string; hostId: string; count: number }) {
  return (
    <div className="v2-group-header">
      <span><HostDot id={hostId} />{title}</span>
      <small>{count}</small>
    </div>
  );
}

export function EntryGroups({ entries, search, favorites = new Set(), onToggleFavorite }: {
  entries: HostedV2Entry[];
  search: string;
  favorites?: ReadonlySet<string>;
  onToggleFavorite?: (hostId: string, recordId: string) => void;
}) {
  const groups = new Map<string, HostedV2Entry[]>();
  entries.forEach((hosted) => {
    const key = `${hosted.host.id}\u001f${hosted.entry.cwd}`;
    groups.set(key, [...(groups.get(key) ?? []), hosted]);
  });
  return [...groups.values()]
    .sort((left, right) => Number(right[0].host.isSelf) - Number(left[0].host.isSelf) || left[0].host.name.localeCompare(right[0].host.name) || left[0].entry.cwd.localeCompare(right[0].entry.cwd, 'zh'))
    .map((items) => <EntryGroupSection items={items} search={search} favorites={favorites} onToggleFavorite={onToggleFavorite} key={`${items[0].host.id}:${items[0].entry.cwd}`} />);
}

function EntryGroupSection({ items, search, favorites, onToggleFavorite }: {
  items: HostedV2Entry[];
  search: string;
  favorites: ReadonlySet<string>;
  onToggleFavorite?: (hostId: string, recordId: string) => void;
}) {
  const [showAll, setShowAll] = useState(false);
  const sorted = [...items].sort((a, b) => Number(Boolean(b.entry.attachment && favorites.has(v2RecordIdentity(b.host.id, b.entry.attachment.recordId)))) - Number(Boolean(a.entry.attachment && favorites.has(v2RecordIdentity(a.host.id, a.entry.attachment.recordId)))) || b.entry.lastActivityAt.localeCompare(a.entry.lastActivityAt));
  return (
    <section className="v2-cwd-group" data-qa="v2-active-group">
      <GroupHeader title={`${cwdLabel(items[0].entry.cwd)} · ${items[0].host.name}`} hostId={items[0].host.id} count={items.length} />
      {(showAll ? sorted : sorted.slice(0, GROUP_PREVIEW_LIMIT)).map((hosted) => {
        const favorite = Boolean(hosted.entry.attachment && favorites.has(v2RecordIdentity(hosted.host.id, hosted.entry.attachment.recordId)));
        return <EntryItem hosted={hosted} search={search} favorite={favorite} onToggleFavorite={onToggleFavorite} key={hosted.entry.entryId} />;
      })}
      {sorted.length > GROUP_PREVIEW_LIMIT && <button className="v2-show-all" data-qa="v2-show-all" type="button" onClick={() => setShowAll((value) => !value)}>{showAll ? '收起' : `查看全部 ${sorted.length}`}</button>}
    </section>
  );
}

export function HistoryGroups({ history, search, favorites = new Set(), onToggleFavorite }: {
  history: HostedV2History[];
  search: string;
  favorites?: ReadonlySet<string>;
  onToggleFavorite?: (hostId: string, recordId: string) => void;
}) {
  const groups = new Map<string, HostedV2History[]>();
  history.forEach((hosted) => {
    const key = `${hosted.host.id}\u001f${hosted.record.cwd}`;
    groups.set(key, [...(groups.get(key) ?? []), hosted]);
  });
  return [...groups.values()]
    .sort((left, right) => Number(right[0].host.isSelf) - Number(left[0].host.isSelf) || left[0].host.name.localeCompare(right[0].host.name) || left[0].record.cwd.localeCompare(right[0].record.cwd, 'zh'))
    .map((items) => <HistoryGroupSection items={items} search={search} favorites={favorites} onToggleFavorite={onToggleFavorite} key={`${items[0].host.id}:${items[0].record.cwd}`} />);
}

function HistoryGroupSection({ items, search, favorites, onToggleFavorite }: {
  items: HostedV2History[];
  search: string;
  favorites: ReadonlySet<string>;
  onToggleFavorite?: (hostId: string, recordId: string) => void;
}) {
  const [showAll, setShowAll] = useState(false);
  const sorted = [...items].sort((a, b) => Number(favorites.has(v2RecordIdentity(b.host.id, b.record.recordId))) - Number(favorites.has(v2RecordIdentity(a.host.id, a.record.recordId))) || b.record.lastActivityAt.localeCompare(a.record.lastActivityAt));
  return (
    <section className="v2-cwd-group" data-qa="v2-history-group">
      <GroupHeader title={`${cwdLabel(items[0].record.cwd)} · ${items[0].host.name}`} hostId={items[0].host.id} count={items.length} />
      {(showAll ? sorted : sorted.slice(0, GROUP_PREVIEW_LIMIT)).map((hosted) => <HistoryItem hosted={hosted} search={search} favorite={favorites.has(v2RecordIdentity(hosted.host.id, hosted.record.recordId))} onToggleFavorite={onToggleFavorite} key={`${hosted.host.id}:${hosted.record.recordId}`} />)}
      {sorted.length > GROUP_PREVIEW_LIMIT && <button className="v2-show-all" data-qa="v2-show-all" type="button" onClick={() => setShowAll((value) => !value)}>{showAll ? '收起' : `查看全部 ${sorted.length}`}</button>}
    </section>
  );
}

export function FavoriteGroup({ entries, history, favorites, search, onToggleFavorite }: {
  entries: HostedV2Entry[];
  history: HostedV2History[];
  favorites: ReadonlySet<string>;
  search: string;
  onToggleFavorite?: (hostId: string, recordId: string) => void;
}) {
  const active = entries.filter(({ host, entry }) => entry.attachment && favorites.has(v2RecordIdentity(host.id, entry.attachment.recordId)));
  const records = history.filter(({ host, record }) => favorites.has(v2RecordIdentity(host.id, record.recordId)));
  return (
    <section className="v2-favorites" data-qa="v2-favorites">
      <h2>★ 收藏 <small>{active.length + records.length}</small></h2>
      {active.map((hosted) => <EntryItem hosted={hosted} search={search} favorite onToggleFavorite={onToggleFavorite} key={hosted.entry.entryId} />)}
      {records.map((hosted) => <HistoryItem hosted={hosted} search={search} favorite onToggleFavorite={onToggleFavorite} key={`${hosted.host.id}:${hosted.record.recordId}`} />)}
    </section>
  );
}

function FourPanes({ entries }: { entries: HostedV2Entry[] }) {
  const change = v2MockView('base').fixture.events.find((event) => event.event === 'attachment_changed');
  const [panes, setPanes] = useState(() => entries.slice(0, 4).map(({ entry }) => entry));
  const applyChange = () => {
    if (change?.event !== 'attachment_changed' || !change.data.attachment) return;
    setPanes((current) => current.map((entry) => entry.entryId === change.data.entryId
      ? applyAttachment(entry, change.data.attachment!, change.data.attachmentRevision)
      : entry));
  };
  return (
    <section className="v2-four-pane-shell" data-qa="v2-four-pane-shell">
      <button type="button" data-qa="v2-apply-attachment" onClick={applyChange}>模拟 attachment_changed</button>
      <div className="v2-four-panes" data-qa="v2-four-panes">
        {panes.map((entry) => (
          <article className="v2-mock-pane" data-entry-id={entry.entryId} key={entry.entryId}>
            <strong>{entry.attachment?.title ?? '未识别对话'}</strong>
            <small>{entry.entryId}</small>
            <p>{entry.lastMessagePreview || '该列保持同一个终端入口身份。'}</p>
          </article>
        ))}
      </div>
    </section>
  );
}

export function V2MockConsole() {
  const location = useLocation();
  const scenario = useScenario();
  const view = useMemo(() => v2MockView(scenario), [scenario]);
  const favorites = useMemo(() => new Set([
    ...view.entries.flatMap(({ host, entry }) => entry.attachment && view.favoriteRecordIds.has(entry.attachment.recordId) ? [v2RecordIdentity(host.id, entry.attachment.recordId)] : []),
    ...view.history.flatMap(({ host, record }) => view.favoriteRecordIds.has(record.recordId) ? [v2RecordIdentity(host.id, record.recordId)] : []),
  ]), [view]);
  const desktop = useDesktopLayout();
  const search = location.search;
  return (
    <main className="v2-console" data-qa="v2-console" data-scenario={scenario}>
      <header className="wa-header v2-header">
        <span><strong>AI 终端</strong><small>窗格优先 v2 · MOCK</small></span>
        <span className="data-mode mock">V2 MOCK</span>
      </header>
      {view.snapshotNotice && <div className="v2-snapshot-notice" data-qa="v2-snapshot-required">{view.snapshotNotice}</div>}
      {desktop && scenario === 'four-panes' ? <FourPanes entries={view.entries} /> : (
        <div className="v2-list-layout">
          <div className="v2-list-scroll">
            <FavoriteGroup entries={view.entries} history={view.history} favorites={favorites} search={search} />
            <section className="v2-zone v2-active-zone" data-qa="v2-active-zone">
              <h1>活跃终端 <small>{view.entries.length}</small></h1>
              <EntryGroups entries={view.entries} search={search} favorites={favorites} />
            </section>
            <section className="v2-zone v2-history-zone" data-qa="v2-history-zone">
              <h1>历史记录 <small>{view.history.length}</small></h1>
              <HistoryGroups history={view.history} search={search} favorites={favorites} />
            </section>
          </div>
          {desktop && <aside className="v2-desktop-preview"><strong>选择终端入口</strong><p>发送始终锚定 entryId；附着记录只负责标题与时间线。</p></aside>}
        </div>
      )}
    </main>
  );
}

export function V2MockTimeline({ kind }: { kind: 'entry' | 'record' }) {
  const { hostId = '', entryId = '', recordId = '' } = useParams();
  const location = useLocation();
  const navigate = useNavigate();
  const scenario = useScenario();
  const view = useMemo(() => v2MockView(scenario), [scenario]);
  const hostedEntry = view.entries.find(({ host, entry }) => host.id === hostId && entry.entryId === entryId);
  const hostedRecord = view.history.find(({ host, record }) => host.id === hostId && record.recordId === recordId);
  const [delivery, setDelivery] = useState('');
  const [text, setText] = useState('');

  if (kind === 'entry' && !hostedEntry) return <main className="v2-timeline"><div className="wa-empty error">终端入口已消失</div></main>;
  if (kind === 'record' && !hostedRecord) return <main className="v2-timeline"><div className="wa-empty error">历史记录不存在</div></main>;

  const entry = hostedEntry?.entry;
  const host = hostedEntry?.host ?? hostedRecord!.host;
  const record = hostedRecord?.record;
  const title = entry?.attachment?.title ?? (entry ? '未识别对话' : record!.title);
  const transportReachable = hostedEntry?.transportState === 'reachable';
  const sendMock = () => {
    if (!entry || !transportReachable || !text.trim()) return;
    const event = view.fixture.events.find((candidate) => candidate.event === 'delivery' && candidate.data.entryId === entry.entryId);
    setDelivery(event?.event === 'delivery' ? `已发送到终端入口 · ${event.data.deliveryId}` : '已发送到终端入口');
    setText('');
  };

  return (
    <main className="v2-timeline" data-qa="v2-timeline" data-entry-id={entry?.entryId ?? ''} data-record-id={record?.recordId ?? entry?.attachment?.recordId ?? ''}>
      <header className="wa-header v2-timeline-header">
        <button type="button" onClick={() => navigate({ pathname: '/', search: location.search })} aria-label="返回终端列表">‹</button>
        <span><strong>{title}</strong><small><HostDot id={host.id} />{host.name} · {entry ? `${entry.kind} · ${entry.pane.paneId}` : '只读历史'}</small></span>
        <span className={entry ? 'v2-entry-badge' : 'v2-history-badge'}>{entry ? '终端入口' : '只读'}</span>
      </header>
      {entry?.attachment?.status === 'suspect' && (
        <details className="v2-attachment-warning" data-qa="v2-attachment-warning">
          <summary>附着的对话可能不符；消息仍会发送到当前终端入口</summary>
          <p>原因：{entry.attachment.suspectReason} · {entry.cwd} · {entry.pane.paneId}</p>
        </details>
      )}
      <section className="v2-timeline-body">
        {entry && !entry.attachment && <div className="v2-unidentified-empty" data-qa="v2-unidentified-empty"><strong>尚未识别对话</strong><span>仍可向此终端发送；记录附着后将在这里显示时间线。</span></div>}
        {entry?.attachment && <div className="v2-mock-message"><p>{entry.lastMessagePreview || '已附着对话时间线。'}</p><small>attachmentRevision {entry.attachmentRevision}</small></div>}
        {record && <div className="v2-mock-message"><p>{record.preview}</p><small>{record.lastActivityAt}</small></div>}
        {delivery && <div className="v2-delivery-accepted" data-qa="v2-delivery-accepted">{delivery}</div>}
      </section>
      {entry ? (
        <footer className="v2-mock-composer">
          {hostedEntry?.transportState !== 'reachable' && <div className="v2-transport-warning">主机不可达，输入暂时禁用</div>}
          <textarea data-qa="v2-chat-input" value={text} disabled={!transportReachable} onChange={(event) => setText(event.target.value)} placeholder="发消息到这个终端入口" />
          <button data-qa="v2-send" type="button" disabled={!transportReachable || !text.trim()} onClick={sendMock}>发送</button>
        </footer>
      ) : <footer className="v2-history-readonly">历史记录只读</footer>}
    </main>
  );
}
