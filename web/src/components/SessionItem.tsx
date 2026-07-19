import { useState } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { sessionCanWrite, type HostedSession, type SessionState } from '../api';
import { hostIdentityColor, sessionIdentity, toggleFavorite, useFavorites } from '../favorites';
import { KillSessionDialog } from './KillSessionDialog';

const STATE_LABELS: Record<SessionState, string> = {
  running: '运行中',
  waiting_input: '等待输入',
  idle: '空闲',
  gone: '已结束',
};

function formatTime(value: string): string {
  const date = new Date(value);
  return new Intl.DateTimeFormat('zh-CN', {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(date);
}

export function SessionItem({
  session,
  selected = false,
  onOpen,
  onSplit,
  onKilled,
  splitDisabled = false,
  showHostIdentity = false,
  contextLabel,
}: {
  session: HostedSession;
  selected?: boolean;
  onOpen?: () => void;
  onSplit?: () => void;
  onKilled?: () => void;
  splitDisabled?: boolean;
  showHostIdentity?: boolean;
  contextLabel?: string;
}) {
  const navigate = useNavigate();
  const location = useLocation();
  const [menuOpen, setMenuOpen] = useState(false);
  const [killOpen, setKillOpen] = useState(false);
  const favorites = useFavorites();
  const favorite = favorites.has(sessionIdentity(session));
  const canFavorite = !session.v2 || Boolean(session.v2.recordId);
  const writable = sessionCanWrite(session);

  const item = (
    <button
      className={`session-item state-${session.state}${!session.live ? ' offline' : ''}${selected ? ' selected' : ''}`}
      type="button"
      onClick={() => onOpen ? onOpen() : navigate(`/sessions/${encodeURIComponent(session.sourceHost.id)}/${encodeURIComponent(session.id)}${location.search}`)}
      data-qa="session-item"
      data-session-id={session.id}
      data-can-send={writable}
      data-favorite={favorite}
      data-live={session.live}
      data-cwd={session.cwd}
    >
      {showHostIdentity && (
        <span
          className="host-identity-dot favorite-host-dot"
          data-qa="host-identity-dot"
          data-host-id={session.sourceHost.id}
          data-host-color={hostIdentityColor(session.sourceHost.id)}
          style={{ backgroundColor: hostIdentityColor(session.sourceHost.id) }}
          title={session.sourceHost.name}
        />
      )}
      <span className={`session-kind kind-${session.kind}`} aria-hidden="true">
        {session.kind === 'claude' ? 'C' : 'X'}
      </span>
      <span className="session-copy">
        <span className="session-title-row">
          <span className="session-title">{session.title}</span>
          <span className={`kind-badge kind-${session.kind}`}>{session.kind}</span>
        </span>
        <span className="session-preview">
          <span className={`state-dot state-${session.state}`} />
          <span className="state-label">{STATE_LABELS[session.state]}</span>
          {contextLabel && <span className="session-project">{contextLabel}</span>}
          <span className="preview-text">{session.lastMessagePreview}</span>
        </span>
      </span>
      <span className="session-meta">
        <time dateTime={session.lastActivityAt}>{formatTime(session.lastActivityAt)}</time>
        {!session.live
          ? <span className="offline-label" data-qa="favorite-offline">离线</span>
          : !writable && <span className="read-only" data-qa="session-read-only" title="当前不可写入">只读</span>}
      </span>
    </button>
  );

  return (
    <div
      className="session-item-shell"
      onContextMenu={(event) => { event.preventDefault(); setMenuOpen(true); }}
      onMouseLeave={() => setMenuOpen(false)}
    >
      {item}
      <button
        className="session-menu-trigger"
        data-qa="session-menu-trigger"
        type="button"
        onClick={() => setMenuOpen((value) => !value)}
        aria-label={`${session.title} 更多操作`}
        aria-expanded={menuOpen}
      >⋮</button>
      {menuOpen && (
        <div className="session-menu" data-qa="session-menu">
          <button
            data-qa="toggle-favorite"
            type="button"
            disabled={!canFavorite}
            onClick={() => { toggleFavorite(session); setMenuOpen(false); }}
          >{!canFavorite ? '未识别入口不可收藏' : favorite ? '取消收藏' : '收藏'}</button>
          {onSplit && (
            <button
              data-qa="split-session"
              type="button"
              disabled={splitDisabled}
              onClick={() => { onSplit(); setMenuOpen(false); }}
            >{splitDisabled ? '已达到 4 列上限' : '分裂展示'}</button>
          )}
          <button
            className="danger-menu-item"
            data-qa="kill-session"
            type="button"
            disabled={!writable}
            onClick={() => { setKillOpen(true); setMenuOpen(false); }}
          >彻底关闭 CLI</button>
        </div>
      )}
      <KillSessionDialog session={session} open={killOpen} onClose={() => setKillOpen(false)} onKilled={onKilled} />
    </div>
  );
}
