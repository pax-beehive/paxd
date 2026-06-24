# internal/harnessregistry BDD test cases

These scenarios define expected behavior for local harness discovery and inventory caching.

`harnessregistry` is a local observation/cache module. It should detect harness availability and write `harness_inventory`, but it must not install adapters or create desired agent connections.

## Module boundaries

Upstream callers:

- `control.Service` calls registry methods for `harnesses.list` and `harnesses.discover` queries.
- `localsessions` may use harness/scanner knowledge when selecting local session scanners.
- daemon background refresh may call discover periodically.

Downstream dependencies:

- detector implementations for Codex, Claude Code, Gemini, and future adapters.
- daemonstore harness inventory repository.
- process/command lookup abstraction for probing.

What to mock in `harnessregistry` tests:

- Mock detectors for registry coordination tests.
- Mock store for cache read/write tests.
- Mock command lookup/probe for harness-specific detector tests.
- Do not use real control service, supervisors, or agent connections.

Who uses `harnessregistry` test helpers:

- `control` tests may use a fake registry port.
- `localsessions` tests may use fake scanner availability based on harness names.

Boundary rule:

- If a test asserts harness detection/cache behavior, it belongs here.
- If a test asserts a user command adopting a harness into an agent connection, it belongs in `control`.
- If a test asserts local session timeline parsing, it belongs in `localsessions`.

## Cached inventory

### Scenario: lists cached harness inventory

Given the store contains cached Codex and Gemini harness statuses  
When `Registry.ListCached` is called  
Then it returns the cached statuses  
And it does not run detectors

### Scenario: empty cache returns empty list

Given the store has no harness inventory  
When `Registry.ListCached` is called  
Then it returns an empty list  
And no error

## Discovery

### Scenario: discover runs all detectors by default

Given detectors are registered for Codex, Claude Code, and Gemini  
When `Registry.Discover` is called without names  
Then each detector is run  
And their normalized statuses are returned  
And the store cache is refreshed

### Scenario: discover can filter detector names

Given detectors are registered for multiple harnesses  
When `Registry.Discover` is called with names `["codex"]`  
Then only the Codex detector is run  
And only Codex status is returned

### Scenario: detector error becomes degraded status

Given a detector returns an error while probing  
When discovery runs  
Then the returned harness status is `degraded` or `missing` according to detector policy  
And `last_error` contains a safe error message  
And discovery continues for other detectors

### Scenario: discover does not install adapters

Given Codex ACP adapter is missing  
When discovery runs  
Then registry returns missing status and install hint  
And it does not run install commands

### Scenario: discover does not create agent connections

Given Codex is available  
When discovery runs  
Then `harness_inventory` may be updated  
And no `agent_connections` desired state is created

## Detector behavior

### Scenario: available command returns available status

Given command lookup finds the configured adapter executable  
When the detector runs  
Then it returns state `available`  
And includes resolved command

### Scenario: missing command returns missing status

Given command lookup does not find the adapter executable  
When the detector runs  
Then it returns state `missing`  
And includes install hint

### Scenario: probe disabled uses lightweight detection

Given discovery request has `Probe=false`  
When a detector supports expensive probes  
Then it skips expensive probe work  
And returns installed/available state based on lightweight checks

## Secret safety

### Scenario: detector output does not expose secrets

Given a detector inspects environment or config  
When it returns status  
Then status contains safe metadata only  
And does not expose token or secret values
