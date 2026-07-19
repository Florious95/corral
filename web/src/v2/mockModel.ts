import rawFixture from '../../mock/v2-fixture.json';
import type {
  V2Attachment,
  V2HistoryRecord,
  V2Host,
  V2MockFixture,
  V2Snapshot,
  V2TerminalEntry,
  V2TransportState,
} from './types';

export interface HostedV2Entry {
  host: V2Host;
  entry: V2TerminalEntry;
  transportState: V2TransportState;
}

export interface HostedV2History {
  host: V2Host;
  record: V2HistoryRecord;
}

export interface V2MockView {
  fixture: V2MockFixture;
  snapshots: V2Snapshot[];
  entries: HostedV2Entry[];
  history: HostedV2History[];
  favoriteRecordIds: ReadonlySet<string>;
  snapshotNotice: string;
}

function cloneFixture(): V2MockFixture {
  return structuredClone(rawFixture) as V2MockFixture;
}

function attachmentEvent(fixture: V2MockFixture) {
  return fixture.events.find((event) => event.event === 'attachment_changed');
}

function removeEntry(fixture: V2MockFixture, entryId: string, retainRecord: boolean): void {
  for (const snapshot of fixture.snapshots) {
    const entry = snapshot.entries.find((candidate) => candidate.entryId === entryId);
    if (!entry) continue;
    snapshot.entries = snapshot.entries.filter((candidate) => candidate.entryId !== entryId);
    const recordId = entry.attachment?.recordId;
    if (retainRecord && recordId && fixture.recordsById[recordId]) snapshot.history.push(fixture.recordsById[recordId]);
  }
}

function applyScenario(fixture: V2MockFixture, scenario: string): string {
  if (scenario === 'attachment-changed') {
    const event = attachmentEvent(fixture);
    if (event?.event === 'attachment_changed') {
      for (const snapshot of fixture.snapshots) {
        const entry = snapshot.entries.find((candidate) => candidate.entryId === event.data.entryId);
        if (!entry) continue;
        entry.attachment = event.data.attachment;
        entry.attachmentRevision = event.data.attachmentRevision;
        entry.lastMessagePreview = 'attachment_changed 已应用';
      }
    }
  } else if (scenario === 'entry-removed' || scenario === 'favorite-exited') {
    removeEntry(fixture, 'e1_AAAAAAAAAAAAAAAAAAAAAA', true);
  } else if (scenario === 'generation-reuse') {
    removeEntry(fixture, 'e1_DDDDDDDDDDDDDDDDDDDDDD', false);
    fixture.snapshots[0]?.entries.push(fixture.replacementEntry);
  } else if (scenario === 'dedupe') {
    fixture.snapshots[0]?.history.push(fixture.duplicateHistoryProbe);
  } else if (scenario === 'snapshot-required') {
    const event = fixture.hostEvents.find((candidate) => candidate.event === 'snapshot_required');
    return event ? `需要刷新原子快照：${event.data.revision}` : '';
  }
  return '';
}

export function v2MockView(scenario: string): V2MockView {
  const fixture = cloneFixture();
  const snapshotNotice = applyScenario(fixture, scenario);
  const entries = fixture.snapshots.flatMap((snapshot) => snapshot.entries.map((entry) => ({
    host: snapshot.host,
    entry,
    transportState: fixture.transport[snapshot.host.id] ?? 'unknown',
  })));
  const attached = new Set(entries.flatMap(({ host, entry }) => entry.attachment ? [`${host.id}\u001f${entry.attachment.recordId}`] : []));
  const historyById = new Map<string, HostedV2History>();
  for (const snapshot of fixture.snapshots) {
    for (const record of snapshot.history) {
      const key = `${snapshot.host.id}\u001f${record.recordId}`;
      if (!attached.has(key) && !historyById.has(key)) historyById.set(key, { host: snapshot.host, record });
    }
  }
  return {
    fixture,
    snapshots: fixture.snapshots,
    entries,
    history: [...historyById.values()],
    favoriteRecordIds: new Set(fixture.favoriteRecordIds),
    snapshotNotice,
  };
}

export function applyAttachment(entry: V2TerminalEntry, attachment: V2Attachment, attachmentRevision: number): V2TerminalEntry {
  return { ...entry, attachment, attachmentRevision, lastMessagePreview: '附着对话已更新' };
}
