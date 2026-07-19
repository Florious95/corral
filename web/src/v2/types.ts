import type { SessionKind, TimelineEvent } from '../api';

export type V2EntryState = 'running' | 'waiting_input' | 'idle' | 'unknown';
export type V2AttachmentStatus = 'attached' | 'suspect';
export type V2SuspectReason = 'delivery_unattributed' | 'evidence_conflict' | 'record_replaced';
export type V2TransportState = 'reachable' | 'unknown' | 'unreachable';
export type V2DeliveryStatus = 'accepted' | 'echoed' | 'unattributed';

export interface V2Host {
  id: string;
  name: string;
  isSelf?: boolean;
}

export interface V2Attachment {
  recordId: string;
  sessionId: string;
  title: string;
  status: V2AttachmentStatus;
  attachmentRevision: number;
  suspectReason?: V2SuspectReason;
}

export interface V2TerminalEntry {
  entryId: string;
  kind: SessionKind;
  cwd: string;
  state: V2EntryState;
  canSend: true;
  model: string;
  lastActivityAt: string;
  lastMessagePreview: string;
  attachmentRevision: number;
  pane: {
    paneId: string;
    windowName: string;
  };
  attachment: V2Attachment | null;
}

export interface V2HistoryRecord {
  recordId: string;
  sessionId: string;
  kind: SessionKind;
  cwd: string;
  title: string;
  model: string;
  lastActivityAt: string;
  preview: string;
}

export interface V2Snapshot {
  revision: string;
  host: V2Host;
  entries: V2TerminalEntry[];
  history: V2HistoryRecord[];
}

export type V2EntryEvent =
  | {
      event: 'attachment_changed';
      data: {
        entryId: string;
        previousRevision: string;
        revision: string;
        previousRecordId: string | null;
        attachmentRevision: number;
        attachment: V2Attachment | null;
      };
    }
  | {
      event: 'entry_removed';
      data: {
        entryId: string;
        previousRevision: string;
        revision: string;
        reason: 'process_exit' | 'pane_gone' | 'killed' | 'generation_replaced';
      };
    }
  | {
      event: 'delivery';
      data: {
        entryId: string;
        deliveryId: string;
        clientNonce: string;
        status: V2DeliveryStatus;
        recordId?: string | null;
      };
    }
  ;

export type V2HostEvent =
  | {
      event: 'snapshot_changed';
      id: string;
      data: {
        previousRevision: string;
        revision: string;
      };
    }
  | {
      event: 'snapshot_required';
      id: string;
      data: {
        previousRevision: string;
        revision: string;
      };
    };
export interface V2EntryTimeline {
  attachment: V2Attachment | null;
  attachmentRevision?: number;
  events: TimelineEvent[];
}

export interface V2WriteAccepted {
  status: 'accepted';
  entryId: string;
  clientNonce: string;
  deliveryId: string;
}

export interface V2UploadAccepted extends V2WriteAccepted {
  path: string;
}

export interface V2ChooseAccepted extends V2WriteAccepted {
  option: number;
}

export interface V2KillAccepted extends V2WriteAccepted {
  killed: true;
  pids: number[];
}

export interface V2EntryScreen {
  entryId: string;
  cols: number;
  rows: number;
  cursorX: number;
  cursorY: number;
  content: string;
  hash: string;
}

export type V2TerminalKey = '0' | '1' | '2' | '3' | '4' | '5' | '6' | '7' | '8' | '9'
  | 'Enter' | 'Up' | 'Down' | 'Left' | 'Right' | 'Escape' | 'Tab' | 'Ctrl+C';

export interface V2KeyAccepted extends V2WriteAccepted {
  key: V2TerminalKey;
}

export interface V2HttpCase {
  entryId: string;
  status: 404 | 410;
  body: {
    error: {
      code: string;
      message: string;
      retryable: boolean;
    };
  };
}

export interface V2MockFixture {
  snapshots: V2Snapshot[];
  transport: Record<string, V2TransportState>;
  favoriteRecordIds: string[];
  recordsById: Record<string, V2HistoryRecord>;
  events: V2EntryEvent[];
  hostEvents: V2HostEvent[];
  replacementEntry: V2TerminalEntry;
  duplicateHistoryProbe: V2HistoryRecord;
  httpCases: V2HttpCase[];
}
