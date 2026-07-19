import type { TimelineEvent } from '../src/api';

export const deepTimelineFixture: TimelineEvent[] = Array.from({ length: 320 }, (_, index) => ({
  seq: index + 1,
  ts: new Date(Date.UTC(2026, 6, 18, 0, 0, index)).toISOString(),
  type: index % 2 === 0 ? 'user_message' as const : 'assistant_message' as const,
  text: `真实分页夹具事件 ${index + 1}`,
}));
