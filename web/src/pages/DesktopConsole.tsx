import { useEffect, useMemo, useState } from 'react';
import { useLocation, useNavigate, useParams } from 'react-router-dom';
import { isMockMode } from '../api';
import { SessionItem } from '../components/SessionItem';
import { hostIdentityColor, rememberFavoriteSessions, useFavorites } from '../favorites';
import { useSessions } from '../hooks/useSessions';
import { GROUP_PREVIEW_LIMIT, favoriteSessions, groupSessions, type SessionGroup } from '../sessionGroups';
import { SessionTimeline } from './SessionTimeline';

interface HostTree {
  id: string;
  name: string;
  groups: SessionGroup[];
  loading: boolean;
  isSelf: boolean;
}

interface TimelinePane {
  hostId: string;
  sessionId: string;
}

function paneKey(pane: TimelinePane): string {
  return `${pane.hostId}:${pane.sessionId}`;
}

export function DesktopConsole() {
  const { hostId, id } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  const { sessions, failedHosts, loadingHosts } = useSessions();
  const favorites = useFavorites();
  useEffect(() => rememberFavoriteSessions(sessions, favorites), [sessions, favorites]);
  const groups = useMemo(() => groupSessions(sessions, favorites), [sessions, favorites]);
  const favoriteGroup = useMemo<SessionGroup>(() => {
    const visible = favoriteSessions(sessions, favorites);
    return {
      key: '__favorites__', cwd: '', cwdLabel: '收藏', hostId: '', hostName: '', hostIsSelf: true,
      sessions: visible, hasActive: true, lastActivityAt: visible[0]?.lastActivityAt ?? '',
    };
  }, [sessions, favorites]);
  const hosts = useMemo(() => {
    const byId = new Map<string, HostTree>();
    for (const group of groups) {
      const host = byId.get(group.hostId) ?? { id: group.hostId, name: group.hostName, groups: [], loading: false, isSelf: group.hostIsSelf };
      host.groups.push(group);
      byId.set(group.hostId, host);
    }
    for (const loadingHost of loadingHosts) {
      const host = byId.get(loadingHost.id) ?? { id: loadingHost.id, name: loadingHost.name, groups: [], loading: true, isSelf: loadingHost.isSelf === true };
      host.loading = true;
      byId.set(loadingHost.id, host);
    }
    return [...byId.values()].sort((a, b) => Number(b.isSelf) - Number(a.isSelf) || a.name.localeCompare(b.name));
  }, [groups, loadingHosts]);
  const routeGroup = groups.find((group) => group.sessions.some((session) => session.sourceHost.id === hostId && session.id === id));
  const [selectedGroupKey, setSelectedGroupKey] = useState<string>();
  const [showAll, setShowAll] = useState(false);
  const [treeHidden, setTreeHidden] = useState(false);
  const [sessionsHidden, setSessionsHidden] = useState(false);
  const [panes, setPanes] = useState<TimelinePane[]>(() => hostId && id ? [{ hostId, sessionId: id }] : []);
  const [focusedPaneKey, setFocusedPaneKey] = useState(() => hostId && id ? paneKey({ hostId, sessionId: id }) : '');
  const [paneNotice, setPaneNotice] = useState('');
  const activeGroupKey = selectedGroupKey ?? routeGroup?.key;
  const selectedGroup = activeGroupKey === favoriteGroup.key ? favoriteGroup : groups.find((group) => group.key === activeGroupKey) ?? groups[0];
  const visibleSessions = showAll ? selectedGroup?.sessions : selectedGroup?.sessions.slice(0, GROUP_PREVIEW_LIMIT);

  useEffect(() => {
    if (!hostId || !id) return;
    setFocusedPaneKey(paneKey({ hostId, sessionId: id }));
    setPanes((current) => current[0]?.hostId === hostId && current[0]?.sessionId === id
      ? current
      : [{ hostId, sessionId: id }]);
  }, [hostId, id]);

  const addPane = (next: TimelinePane) => {
    if (panes.some((pane) => paneKey(pane) === paneKey(next))) {
      setFocusedPaneKey(paneKey(next));
      setPaneNotice('');
      return;
    }
    if (panes.length >= 4) {
      setPaneNotice('已达 4 列上限，请先关闭一列');
      return;
    }
    setPanes([...panes, next]);
    setFocusedPaneKey(paneKey(next));
    setPaneNotice('');
  };

  const openFromList = (next: TimelinePane) => {
    if (panes.length < 2) {
      navigate(`/sessions/${encodeURIComponent(next.hostId)}/${encodeURIComponent(next.sessionId)}${location.search}`);
      return;
    }
    addPane(next);
  };

  const closePane = (pane: TimelinePane) => {
    const next = panes.filter((item) => item.hostId !== pane.hostId || item.sessionId !== pane.sessionId);
    setPanes(next);
    if (focusedPaneKey === paneKey(pane)) setFocusedPaneKey(next[0] ? paneKey(next[0]) : '');
    setPaneNotice('');
    if (pane.hostId !== hostId || pane.sessionId !== id) return;
    const first = next[0];
    navigate(first
      ? `/sessions/${encodeURIComponent(first.hostId)}/${encodeURIComponent(first.sessionId)}${location.search}`
      : `/${location.search}`);
  };

  return (
    <main className={`desktop-console${treeHidden ? ' tree-hidden' : ''}${sessionsHidden ? ' sessions-hidden' : ''}`} data-qa="desktop-console">
      {!treeHidden && <aside className="desktop-tree">
        <header className="desktop-brand">
          <span>AI 会话</span>
          <span className={`data-mode ${isMockMode() ? 'mock' : 'live'}`}>{isMockMode() ? 'MOCK' : 'LIVE'}</span>
        </header>
        <div className="desktop-tree-scroll">
          <button
            className={`desktop-favorites${selectedGroup?.key === favoriteGroup.key ? ' selected' : ''}`}
            data-qa="favorites-folder"
            type="button"
            onClick={() => { setSelectedGroupKey(favoriteGroup.key); setShowAll(false); }}
          >
            <span>★ 收藏</span>
            <small>{favoriteGroup.sessions.length}</small>
          </button>
          {hosts.map((host) => (
            <section className="desktop-host" data-host-id={host.id} key={host.id}>
              <div className="desktop-host-name">
                <span className="host-identity-dot" data-qa="host-identity-dot" data-host-id={host.id} data-host-color={hostIdentityColor(host.id)} style={{ backgroundColor: hostIdentityColor(host.id) }} />
                <span>{host.name}</span>
                {host.loading && <small data-qa="desktop-host-loading">加载中…</small>}
              </div>
              {host.groups.map((group) => (
                <button
                  className={`desktop-cwd${selectedGroup?.key === group.key ? ' selected' : ''}`}
                  data-qa="desktop-cwd"
                  data-host-id={group.hostId}
                  data-cwd-label={group.cwdLabel}
                  type="button"
                  key={group.key}
                  onClick={() => { setSelectedGroupKey(group.key); setShowAll(false); }}
                  title={group.cwd}
                >
                  <span>{group.cwdLabel}</span>
                  <small>{group.sessions.length}</small>
                </button>
              ))}
            </section>
          ))}
        </div>
      </aside>}

      {!sessionsHidden && <section className="desktop-session-column">
        <header className="desktop-column-header">
          <span>{selectedGroup?.cwdLabel ?? '会话'}</span>
          <small>{selectedGroup?.sessions.length ?? 0} 个会话</small>
        </header>
        <div className="aggregation-warning-slot" aria-live="polite">
          {failedHosts.length > 0 && <div className="aggregation-warning" data-qa="aggregation-warning">部分主机暂不可达：{failedHosts.join('、')}</div>}
        </div>
        <div className="desktop-session-list">
          {visibleSessions?.map((session) => (
            <SessionItem
              key={`${session.sourceHost.id}:${session.id}`}
              session={session}
              selected={panes.some((pane) => pane.hostId === session.sourceHost.id && pane.sessionId === session.id)}
              onOpen={() => openFromList({ hostId: session.sourceHost.id, sessionId: session.id })}
              onSplit={() => addPane({ hostId: session.sourceHost.id, sessionId: session.id })}
              onKilled={() => closePane({ hostId: session.sourceHost.id, sessionId: session.id })}
              splitDisabled={panes.length >= 4 && !panes.some((pane) => pane.hostId === session.sourceHost.id && pane.sessionId === session.id)}
              showHostIdentity={selectedGroup?.key === favoriteGroup.key}
            />
          ))}
          {selectedGroup && selectedGroup.sessions.length > GROUP_PREVIEW_LIMIT && (
            <button className="show-all-sessions" type="button" onClick={() => setShowAll((value) => !value)}>
              {showAll ? `收起至前 ${GROUP_PREVIEW_LIMIT} 条` : `查看全部（另 ${selectedGroup.sessions.length - GROUP_PREVIEW_LIMIT} 条）`}
            </button>
          )}
        </div>
      </section>}

      <section className="desktop-timeline">
        <div className="desktop-layout-controls" data-qa="desktop-layout-controls">
          <button type="button" data-qa="toggle-host-tree" className={treeHidden ? '' : 'active'} onClick={() => setTreeHidden((value) => !value)} aria-label={`${treeHidden ? '显示' : '隐藏'}主机栏`}>主机</button>
          <button type="button" data-qa="toggle-session-list" className={sessionsHidden ? '' : 'active'} onClick={() => setSessionsHidden((value) => !value)} aria-label={`${sessionsHidden ? '显示' : '隐藏'}会话栏`}>会话</button>
          {paneNotice && <span className="pane-limit-notice" data-qa="pane-limit-notice" role="status">{paneNotice}</span>}
          <span className="pane-count">{panes.length > 1 ? `${panes.length} 列时间线` : '时间线'}</span>
        </div>
        {panes.length > 0
          ? (
            <div className={`desktop-timeline-panes panes-${panes.length}`} data-qa="timeline-panes" data-pane-count={panes.length}>
              {panes.map((pane) => (
                <div
                  className={`desktop-pane${focusedPaneKey === paneKey(pane) ? ' focused' : ''}`}
                  data-qa="timeline-pane"
                  data-session-id={pane.sessionId}
                  data-focused={focusedPaneKey === paneKey(pane)}
                  key={paneKey(pane)}
                  onClick={() => setFocusedPaneKey(paneKey(pane))}
                >
                  <SessionTimeline
                    embedded
                    hostId={pane.hostId}
                    sessionId={pane.sessionId}
                    onClose={() => closePane(pane)}
                  />
                </div>
              ))}
            </div>
          )
          : <div className="desktop-no-selection"><span>选择一个会话查看时间线</span></div>}
      </section>
    </main>
  );
}
