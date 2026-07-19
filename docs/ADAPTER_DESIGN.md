# Transcript adapter design

Status: design only. The clean extraction preserves current behavior; adapter migration is a subsequent change.

## Compatibility baseline

- Claude Code: validated against CLI `2.1.181` and its line-delimited project transcripts plus `~/.claude/sessions/<pid>.json` registry.
- Codex CLI: validated against `0.144.1` (and observed transcript `cli_version` `0.144.0`) under `~/.codex/sessions/**/rollout-*.jsonl`.

Adapters must reject unsupported structural changes explicitly. Version strings are diagnostic compatibility evidence, not a promise that every future row shape is compatible.

## Boundary

The adapter owns transcript-format knowledge only:

- source discovery and stable source identity;
- incremental metadata parsing;
- history hints exposed by the CLI format;
- timeline row parsing with page-boundary context;
- appended user-message reconciliation.

The gateway continues to own tmux/process identity, sticky binding, pagination, HTTP/SSE, delivery state, file watching, and send-keys injection. An adapter never selects a pane or mutates a transcript.

## Proposed Go API

```go
package adapter

type Kind string

const (
    ClaudeCode Kind = "claudecode"
    Codex      Kind = "codex"
)

type Source struct {
    Kind Kind
    Path string
    ID   string
}

type ParseCursor struct {
    Offset int64
    Tail   []byte
}

type TimelineContext struct {
    ToolNames map[string]string
    QueuedAt  map[string][]time.Time
}

type Adapter interface {
    Kind() Kind
    Discover(home string) ([]Source, error)
    ParseMetadata(source Source, previous *Record, cursor ParseCursor) (*Record, ParseCursor, error)
    ParseTimeline(reader io.Reader, firstSeq int64, context TimelineContext) ([]Event, TimelineContext, error)
    MatchAppendedUserMessage(data []byte, expected string) (matchKind string, matched bool)
}

type HistoryProvider interface {
    LoadHistoryHints(home string) (map[string]HistoryHint, error)
}
```

`Record`, `Event`, and `HistoryHint` are adapter-neutral DTOs. They must not contain `paneBinding`, HTTP response fields, cache mutexes, or watcher state.

## Migration map

### `adapter/claudecode`

- `sessions.go`: `newClaudeMetadataRecord`, `applyClaudeMetadataLine`, `finalizeClaudeMetadata`, `parseClaudeMetadataFrom`, and `parseClaudeMetadata`.
- `sessions.go`: `claudeTimeline`, `claudeTimelineReader`, and `claudeTimelineReaderWithContext`.
- `history_state.go`: `loadClaudeHistoryIndex` and `observeClaudeHistoryLine` behind `HistoryProvider`.
- `sessions.go`: `waitForClaudeSendReconciliation`, `appendedClaudeUserMessageMatches`, and `appendedClaudeUserMessageMatch`.
- Claude-specific roots currently handled by `discoverPhysicalFiles` move into `Discover`; inode/device dedupe remains shared infrastructure.

### `adapter/codex`

- `sessions.go`: `loadCodexTitles`, `parseCodexMetadata`, `validCodexWindowName`, and the format-specific portion of `applyCodexWindowTitle`.
- `sessions.go`: `codexTimeline` and `codexTimelineReader`.
- `v2_write.go`: `waitForCodexSendReconciliation` and `appendedCodexUserMessageMatches`.
- Codex rollout roots currently handled by `discoverPhysicalFiles` move into `Discover`.

### Remains in the gateway

- `collectAllRecords*`, binding and placeholder creation, state derivation, v1/v2 response shaping, timeline paging/cache/checkpoints, upload storage, SSE, and all pane verification/injection.
- Shared text presentation helpers such as truncation and tool summaries remain outside adapters unless a row shape requires format-specific extraction.

## Migration sequence

1. Freeze characterization tests for current Claude Code and Codex fixtures, including page-boundary tool names, queue rows, and send reconciliation.
2. Add the neutral DTOs and interfaces; initially wrap the existing functions without moving logic.
3. Move Claude Code parsing and its tests, then Codex parsing and its tests. Each move must keep full gateway tests green.
4. Route discovery through the adapter registry and delete the old kind switch only after both adapters pass the same incremental-tail and pagination gates.
5. Add fixture metadata recording the observed CLI version and emit one compact diagnostic for unknown row shapes; do not silently reinterpret them.

This sequence keeps the externally visible API and binding safety model unchanged while isolating CLI-format churn.
