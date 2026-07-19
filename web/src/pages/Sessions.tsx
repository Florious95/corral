import { useEffect, useMemo, useRef, useState } from 'react';
import type { HostedSession } from '../api';
import { SessionItem } from '../components/SessionItem';
import { hostIdentityColor, rememberFavoriteSessions, sessionIdentity, useFavorites } from '../favorites';
import { useSessions } from '../hooks/useSessions';
import { cwdLabel, favoriteSessions, groupSessions, type SessionGroup } from '../sessionGroups';
import { prefetchTimelines } from '../timelineCache';

const EXPANSION_KEY = 'fleet-mobile-browse-expansion-v1';
const EXPANSION_VERSION = 1;

interface ExpansionState {
  hosts: ReadonlySet<string>;
  projects: ReadonlySet<string>;
}

function readExpansion(): ExpansionState {
  try {
    const value = JSON.parse(localStorage.getItem(EXPANSION_KEY) ?? 'null') as { version?: number; hosts?: unknown; projects?: unknown } | null;
    if (value?.version !== EXPANSION_VERSION || !Array.isArray(value.hosts) || !Array.isArray(value.projects)) {
      localStorage.removeItem(EXPANSION_KEY);
      return { hosts: new Set(), projects: new Set() };
    }
    return {
      hosts: new Set(value.hosts.filter((item): item is string => typeof item === 'string')),
      projects: new Set(value.projects.filter((item): item is string => typeof item === 'string')),
    };
  } catch {
    localStorage.removeItem(EXPANSION_KEY);
    return { hosts: new Set(), projects: new Set() };
  }
}

function writeExpansion(value: ExpansionState): void {
  localStorage.setItem(EXPANSION_KEY, JSON.stringify({
    version: EXPANSION_VERSION,
    hosts: [...value.hosts],
    projects: [...value.projects],
  }));
}

function includesQuery(session: HostedSession, query: string): boolean {
  if (!query) return true;
  return session.title.toLocaleLowerCase().includes(query)
    || session.cwd.toLocaleLowerCase().includes(query)
    || cwdLabel(session.cwd).toLocaleLowerCase().includes(query);
}

interface BrowseHost {
  id: string;
  name: string;
  isSelf: boolean;
  groups: SessionGroup[];
}

const ACTIVE_SESSION_ROW_HEIGHT = 74;
const ACTIVE_LABEL_ROW_HEIGHT = 34;
const ACTIVE_OVERSCAN = 5;

type ActiveRow =
  | { key: string; type: 'label'; label: string; count: number; height: number }
  | { key: string; type: 'session'; session: HostedSession; height: number };

function ActiveSessionStream({
  pinned,
  recent,
  scrollRoot,
}: {
  pinned: HostedSession[];
  recent: HostedSession[];
  scrollRoot: HTMLDivElement | null;
}) {
  const containerRef = useRef<HTMLDivElement>(null);
  const rows = useMemo(() => {
    const next: ActiveRow[] = [];
    if (pinned.length > 0) {
      next.push({ key: 'label:pinned', type: 'label', label: '收藏', count: pinned.length, height: ACTIVE_LABEL_ROW_HEIGHT });
      pinned.forEach((session) => next.push({ key: `session:${sessionIdentity(session)}`, type: 'session', session, height: ACTIVE_SESSION_ROW_HEIGHT }));
    }
    if (recent.length > 0) {
      next.push({ key: 'label:recent', type: 'label', label: '最近活动', count: recent.length, height: ACTIVE_LABEL_ROW_HEIGHT });
      recent.forEach((session) => next.push({ key: `session:${sessionIdentity(session)}`, type: 'session', session, height: ACTIVE_SESSION_ROW_HEIGHT }));
    }
    return next;
  }, [pinned, recent]);
  const offsets = useMemo(() => {
    const next: number[] = [];
    let offset = 0;
    rows.forEach((row) => {
      next.push(offset);
      offset += row.height;
    });
    return { values: next, total: offset };
  }, [rows]);
  const [range, setRange] = useState({ start: 0, end: Math.min(rows.length, 20) });

  useEffect(() => {
    const container = containerRef.current;
    if (!scrollRoot || !container) return;
    let frame = 0;
    const measure = () => {
      frame = 0;
      const rootRect = scrollRoot.getBoundingClientRect();
      const containerRect = container.getBoundingClientRect();
      const visibleTop = Math.max(0, rootRect.top - containerRect.top);
      const visibleBottom = visibleTop + scrollRoot.clientHeight;
      let start = 0;
      while (start < rows.length && offsets.values[start] + rows[start].height < visibleTop) start += 1;
      let end = start;
      while (end < rows.length && offsets.values[end] < visibleBottom) end += 1;
      start = Math.max(0, start - ACTIVE_OVERSCAN);
      end = Math.min(rows.length, end + ACTIVE_OVERSCAN);
      setRange((current) => current.start === start && current.end === end ? current : { start, end });
    };
    const scheduleMeasure = () => {
      if (!frame) frame = requestAnimationFrame(measure);
    };
    measure();
    scrollRoot.addEventListener('scroll', scheduleMeasure, { passive: true });
    window.addEventListener('resize', scheduleMeasure, { passive: true });
    return () => {
      if (frame) cancelAnimationFrame(frame);
      scrollRoot.removeEventListener('scroll', scheduleMeasure);
      window.removeEventListener('resize', scheduleMeasure);
    };
  }, [offsets, rows, scrollRoot]);

  return (
    <div
      className="mobile-active-stream mobile-active-virtual"
      data-qa="mobile-active-stream"
      data-total-sessions={pinned.length + recent.length}
      ref={containerRef}
      style={{ height: offsets.total }}
    >
      {rows.slice(range.start, range.end).map((row, index) => (
        <div
          className={`mobile-active-virtual-row row-${row.type}`}
          key={row.key}
          style={{ height: row.height, transform: `translate3d(0, ${offsets.values[range.start + index]}px, 0)` }}
        >
          {row.type === 'label' ? (
            <div className="mobile-stream-label">{row.label} <span>{row.count}</span></div>
          ) : (
            <SessionItem session={row.session} showHostIdentity contextLabel={cwdLabel(row.session.cwd)} />
          )}
        </div>
      ))}
    </div>
  );
}

export function Sessions() {
  const { sessions, err, loading, failedHosts, loadingHosts } = useSessions();
  const favorites = useFavorites();
  const [view, setView] = useState<'active' | 'browse'>('active');
  const [search, setSearch] = useState('');
  const [expansion, setExpansion] = useState(readExpansion);
  const [scrollRoot, setScrollRoot] = useState<HTMLDivElement | null>(null);
  const query = search.trim().toLocaleLowerCase();

  useEffect(() => rememberFavoriteSessions(sessions, favorites), [sessions, favorites]);
  const groups = useMemo(() => groupSessions(sessions, favorites), [sessions, favorites]);
  const activeSessions = useMemo(() => {
    const unique = new Map<string, HostedSession>();
    groups.flatMap((group) => group.sessions).forEach((session) => unique.set(sessionIdentity(session), session));
    favoriteSessions(sessions, favorites).forEach((session) => unique.set(sessionIdentity(session), session));
    return [...unique.values()]
      .filter((session) => includesQuery(session, query))
      .sort((left, right) =>
        Number(favorites.has(sessionIdentity(right))) - Number(favorites.has(sessionIdentity(left)))
        || right.lastActivityAt.localeCompare(left.lastActivityAt));
  }, [groups, sessions, favorites, query]);
  const pinned = activeSessions.filter((session) => favorites.has(sessionIdentity(session)));
  const recent = activeSessions.filter((session) => !favorites.has(sessionIdentity(session)));

  useEffect(() => {
    prefetchTimelines(pinned.filter((session) => session.live));
  }, [pinned]);

  const browseHosts = useMemo(() => {
    const byHost = new Map<string, BrowseHost>();
    for (const group of groups) {
      const projectMatches = group.cwd.toLocaleLowerCase().includes(query) || group.cwdLabel.toLocaleLowerCase().includes(query);
      const visibleSessions = projectMatches ? group.sessions : group.sessions.filter((session) => includesQuery(session, query));
      if (visibleSessions.length === 0) continue;
      const visibleGroup = visibleSessions.length === group.sessions.length ? group : { ...group, sessions: visibleSessions };
      const host = byHost.get(group.hostId) ?? {
        id: group.hostId,
        name: group.hostName,
        isSelf: group.hostIsSelf,
        groups: [],
      };
      host.name = group.hostName;
      host.isSelf ||= group.hostIsSelf;
      host.groups.push(visibleGroup);
      byHost.set(group.hostId, host);
    }
    return [...byHost.values()].sort((left, right) =>
      Number(right.isSelf) - Number(left.isSelf) || left.name.localeCompare(right.name));
  }, [groups, query]);

  const toggleExpansion = (kind: 'hosts' | 'projects', key: string) => {
    setExpansion((current) => {
      const nextSet = new Set(current[kind]);
      if (nextSet.has(key)) nextSet.delete(key);
      else nextSet.add(key);
      const next = { ...current, [kind]: nextSet };
      writeExpansion(next);
      return next;
    });
  };

  return (
    <div className="sessions-page" ref={setScrollRoot}>
      <section className="mobile-session-toolbar" data-qa="mobile-session-toolbar">
        <div className="mobile-view-switch" role="group" aria-label="会话视图">
          <button data-qa="mobile-view-active" type="button" aria-pressed={view === 'active'} onClick={() => setView('active')}>活跃</button>
          <button data-qa="mobile-view-browse" type="button" aria-pressed={view === 'browse'} onClick={() => setView('browse')}>浏览</button>
        </div>
        <input
          className="mobile-session-search"
          data-qa="mobile-session-search"
          type="search"
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="搜索会话或项目"
          aria-label="搜索会话或项目"
        />
      </section>

      {failedHosts.length > 0 && <div className="aggregation-warning-slot" aria-live="polite">
        <div className="aggregation-warning" data-qa="aggregation-warning">部分主机暂不可达：{failedHosts.join('、')}</div>
      </div>}
      {loadingHosts.map((host) => (
        <section className="host-loading" data-qa="host-loading" data-host-id={host.id} key={host.id}>
          <span className="host-loading-spinner" aria-hidden="true" />
          <span className="host-loading-copy"><strong>{host.name}</strong><small>正在加载会话…</small></span>
          <span className="host-badge"><span className="host-identity-dot" data-host-id={host.id} style={{ backgroundColor: hostIdentityColor(host.id) }} />加载中</span>
        </section>
      ))}

      {view === 'active' ? (
        <ActiveSessionStream pinned={pinned} recent={recent} scrollRoot={scrollRoot} />
      ) : (
        <div className="mobile-browse" data-qa="mobile-browse">
          {browseHosts.map((host) => {
            const expanded = Boolean(query) || expansion.hosts.has(host.id);
            const sessionCount = host.groups.reduce((total, group) => total + group.sessions.length, 0);
            return (
              <section className="mobile-host-section" data-qa="mobile-host-section" data-host-id={host.id} key={host.id}>
                <button className="mobile-host-header" data-qa="mobile-host-group" data-host-id={host.id} type="button" aria-expanded={expanded} onClick={() => toggleExpansion('hosts', host.id)}>
                  <span className={`group-chevron${expanded ? ' expanded' : ''}`}>›</span>
                  <span className="host-identity-dot" style={{ backgroundColor: hostIdentityColor(host.id) }} />
                  <span className="mobile-host-copy"><strong>{host.name}</strong><small>{host.groups.length} 个项目 · {sessionCount} 个会话</small></span>
                </button>
                {expanded && <div className="mobile-project-list">
                  {host.groups.map((group) => {
                    const projectExpanded = Boolean(query) || expansion.projects.has(group.key);
                    return (
                      <section className="mobile-project-section" data-qa="mobile-project-section" data-project-key={group.key} key={group.key}>
                        <button className="mobile-project-header" data-qa="mobile-project-group" data-project-key={group.key} type="button" aria-expanded={projectExpanded} title={group.cwd} onClick={() => toggleExpansion('projects', group.key)}>
                          <span className={`group-chevron${projectExpanded ? ' expanded' : ''}`}>›</span>
                          <span className="group-copy"><span className="group-cwd">{group.cwdLabel}</span><span className="group-count">{group.sessions.length} 个会话</span></span>
                        </button>
                        {projectExpanded && <div className="group-sessions">{group.sessions.map((session) => (
                          <SessionItem key={`${session.sourceHost.id}:${session.id}`} session={session} />
                        ))}</div>}
                      </section>
                    );
                  })}
                </div>}
              </section>
            );
          })}
        </div>
      )}

      {loading && groups.length === 0 && loadingHosts.length === 0 && <div className="wa-empty">正在读取会话…</div>}
      {!loading && activeSessions.length === 0 && !err && <div className="wa-empty">{query ? '没有匹配的会话或项目' : '暂无 Claude / Codex 会话'}</div>}
      {err && <div className="wa-empty error">无法读取会话：{err}</div>}
    </div>
  );
}
