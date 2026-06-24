# internal/localsessions BDD test cases

These scenarios define expected behavior for local-only session observation.

`localsessions` manages local session metadata and optional timeline cache for TUI/CLI. It does not require a remote and must not mutate desired agent connection state.

## Module boundaries

Upstream callers:

- `control.Service` calls this module for local session list/sync/get queries.
- TUI and paxctl reach this module indirectly through the local control API.

Downstream dependencies:

- local session store repository for `local_sessions` and `local_session_elements`.
- scanner registry for agent-specific scanners.
- harness/local agent file or process adapters through scanners.

What to mock in `localsessions` tests:

- Mock scanner registry and scanners for service-level tests.
- Use in-memory or temporary SQLite local session store for cache behavior.
- Do not use real localapi, controlws, remotes, agent connections, or ACP tunnel runtime.

Who uses `localsessions` test helpers:

- `control` tests may use a fake localsessions port.
- TUI/client tests may use local session response fixtures from `internal/testkit/controltest`.

Boundary rule:

- If a test asserts local session cache/list/sync/get behavior, it belongs here.
- If a test asserts UI rendering, it belongs in the TUI module.
- If a test asserts remote status or tunnel behavior, it belongs in supervisor/runtime.

## Listing

### Scenario: lists cached sessions

Given local session cache contains Codex sessions  
When `Service.List` is called with agent `codex`  
Then it returns matching sessions ordered by recent activity  
And it does not rescan local files unless explicitly requested

### Scenario: list with no agent returns all agents

Given local session cache contains Codex and Gemini sessions  
When `Service.List` is called without agent filter  
Then it returns sessions across agents  
And applies the requested limit

### Scenario: empty cache returns empty list

Given no local sessions are cached  
When `Service.List` is called  
Then it returns an empty list  
And no error

## Sync

### Scenario: sync scans available agents

Given scanner registry reports Codex scanner available  
And the scanner returns two sessions  
When `Service.Sync` is called for Codex  
Then sessions are upserted into cache  
And sync result reports two synced sessions

### Scenario: sync skips unavailable agent when not explicit

Given scanner registry has no scanner for Gemini  
When `Service.Sync` is called without explicit agent filter  
Then Gemini is skipped  
And sync continues for available agents

### Scenario: sync explicit unavailable agent returns error

Given scanner registry has no scanner for requested agent `gemini`  
When `Service.Sync` is called with agent `gemini`  
Then sync returns an error  
And no cache rows are written for Gemini

### Scenario: sync timeout is passed to scanner

Given a sync query includes timeout  
When scanner work starts  
Then scanner context respects the timeout  
And timeout errors are reported in sync result

## Session detail

### Scenario: get cached session detail

Given a session and elements are cached  
When `Service.Get` is called for the session id  
Then it returns session metadata and ordered elements

### Scenario: get missing session returns not found

Given no session exists for the requested id  
When `Service.Get` is called  
Then it returns a not found error

### Scenario: get can lazy-load elements when missing

Given session metadata is cached but elements are not synced  
And the scanner supports loading elements  
When `Service.Get` is called according to lazy-load policy  
Then it loads elements through the scanner  
And updates `local_session_elements`

## Store behavior

### Scenario: upsert session preserves stable id

Given scanner returns native id `sess_1` for agent `codex`  
When sessions are upserted  
Then local session id is `codex:sess_1`  
And repeated upserts update metadata without duplicating rows

### Scenario: replacing elements removes stale elements

Given a session has cached elements with seq 1 through 5  
When sync replaces elements with seq 1 through 3  
Then only seq 1 through 3 are returned later

## Boundaries

### Scenario: local session sync does not require a remote

Given there are no configured remotes  
When local session sync runs  
Then it can still scan local agents  
And it does not create a `remote=localhost`

### Scenario: local session sync does not create agent connections

Given Codex sessions are discovered locally  
When sync completes  
Then no `agent_connections` rows are created  
And no supervisor wake is required

### Scenario: local session results do not expose secrets

Given local session metadata contains environment-derived details  
When sessions are returned  
Then results include safe metadata only  
And do not include secrets or tokens
