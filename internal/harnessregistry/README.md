# internal/harnessregistry

`internal/harnessregistry` discovers local agent harnesses and adapters.

It supports local observation, TUI display, and optional remote reporting.

This package owns:

- local harness inventory
- command resolution
- availability probing
- install hints
- cheap local session discovery when supported

Initial harnesses:

- Codex
- Claude Code
- Gemini
- Kimi Code
- Pi
- future ACP-compatible adapters

This package must not:

- silently install adapters
- automatically adopt discovered harnesses into remote agent connections
- write desired agent connections directly
- expose resolved secrets

Discovery may update `harness_inventory`, but it is a cache refresh, not a desired-state command.

## Primary Interfaces

```go
type Registry interface {
    ListCached(ctx context.Context) ([]HarnessStatus, error)
    Discover(ctx context.Context, req DiscoverRequest) ([]HarnessStatus, error)
}

type Detector interface {
    Detect(ctx context.Context) (HarnessStatus, error)
}

type Store interface {
    ListHarnesses(ctx context.Context) ([]HarnessStatus, error)
    UpsertHarnesses(ctx context.Context, statuses []HarnessStatus) error
}

type DiscoverRequest struct {
    Probe bool
    Names []string
}

type HarnessStatus struct {
    Harness     string
    DisplayName string
    State       string // available | missing | degraded
    Capability  string // acp | local-log | gateway
    Command     []string
    Version     string
    Source      string // native | adapter | npm | local
    InstallHint string
    LastError   string
}
```

Detector implementations should be small and harness-specific. The registry coordinates detectors, cache reads/writes, and error normalization.

## Boundary

Harness discovery is a cache refresh and local observation feature. It must not create `agent_connections` or install adapters without an explicit control command.
