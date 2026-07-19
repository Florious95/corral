import type { HostedSession } from './api';
import { getFavoriteSnapshot, sessionIdentity } from './favorites';

// 真实近 7 天会话可达 195+；每个列表首屏只挂载前 20 条。
export const GROUP_PREVIEW_LIMIT = 20;

export interface SessionGroup {
  key: string;
  cwd: string;
  cwdLabel: string;
  hostId: string;
  hostName: string;
  hostIsSelf: boolean;
  sessions: HostedSession[];
  hasActive: boolean;
  lastActivityAt: string;
}

export function cwdLabel(cwd: string): string {
  const parts = cwd.split('/').filter(Boolean);
  return parts.slice(-2).join(' / ') || cwd;
}

export function groupSessions(sessions: HostedSession[], favorites: ReadonlySet<string> = new Set()): SessionGroup[] {
  const groups = new Map<string, SessionGroup>();

  for (const session of sessions) {
    if (!session.live) continue;
    const key = `${session.sourceHost.id}:${session.cwd}`;
    let group = groups.get(key);
    if (!group) {
      group = {
        key,
        cwd: session.cwd,
        cwdLabel: cwdLabel(session.cwd),
        hostId: session.sourceHost.id,
        hostName: session.sourceHost.name,
        hostIsSelf: session.sourceHost.isSelf === true,
        sessions: [],
        hasActive: false,
        lastActivityAt: session.lastActivityAt,
      };
      groups.set(key, group);
    }
    group.sessions.push(session);
    group.hasActive ||= session.state === 'running' || session.state === 'waiting_input';
    if (session.lastActivityAt > group.lastActivityAt) group.lastActivityAt = session.lastActivityAt;
  }

  return [...groups.values()]
    .map((group) => ({
      ...group,
      sessions: group.sessions.sort((a, b) =>
        Number(favorites.has(sessionIdentity(b))) - Number(favorites.has(sessionIdentity(a)))
        || b.lastActivityAt.localeCompare(a.lastActivityAt)),
    }))
    .sort((a, b) =>
      Number(b.hostIsSelf) - Number(a.hostIsSelf)
      || a.hostName.localeCompare(b.hostName)
      || a.cwdLabel.localeCompare(b.cwdLabel, 'zh')
      || a.cwd.localeCompare(b.cwd, 'zh'));
}

export function favoriteSessions(sessions: HostedSession[], favorites: ReadonlySet<string>): HostedSession[] {
  const current = new Map(sessions.map((session) => [sessionIdentity(session), session]));
  return [...favorites]
    .map((identity) => {
      const active = current.get(identity);
      if (active) return active;
      const snapshot = getFavoriteSnapshot(identity);
      return snapshot ? { ...snapshot, live: false, canSend: false, state: 'gone' as const } : undefined;
    })
    .filter((session): session is HostedSession => session !== undefined)
    .sort((a, b) => a.title.localeCompare(b.title, 'zh') || b.lastActivityAt.localeCompare(a.lastActivityAt));
}
