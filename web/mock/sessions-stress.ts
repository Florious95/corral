const sessions = Array.from({ length: 160 }, (_, index) => ({
  id: `s-stress-${String(index + 1).padStart(3, '0')}`,
  kind: index % 2 === 0 ? 'claude' as const : 'codex' as const,
  cwd: '/Users/demo/Documents/code/large-fleet-project',
  title: `压力会话 ${String(index + 1).padStart(3, '0')}`,
  sessionId: `stress-session-${String(index + 1).padStart(3, '0')}`,
  sessionFile: `/Users/demo/.mock/stress-session-${index + 1}.jsonl`,
  state: index === 0 ? 'running' as const : 'idle' as const,
  canSend: true,
  model: index % 2 === 0 ? 'claude-opus-4-7' : 'gpt-5.4',
  lastActivityAt: new Date(Date.UTC(2026, 6, 14, 3, 20) - index * 60_000).toISOString(),
  lastMessagePreview: `用于验证 150+ 会话列表首屏渲染，第 ${index + 1} 条。`,
  live: true,
}));

export const stressSessionsFixture = {
  hosts: [{ id: 'host-stress', name: 'Stress Host', sessions }],
};
