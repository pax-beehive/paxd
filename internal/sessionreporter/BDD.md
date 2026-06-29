# internal/sessionreporter BDD test cases

These scenarios define expected behavior for best-effort reporting of local
agent sessions to pax-manager.

`sessionreporter` observes running agent connection runtimes, scans local session
sources, and reports session-only payloads to the remote manager. It does not
own desired agent connection state, does not start or stop agent tunnels, and
does not update local session cache rows for TUI/CLI.

## Module boundaries

Upstream callers:

- daemon bootstrap starts the reporter with the daemon root context.
- the reporter reads observed agent runtimes from the agent connection supervisor.

Downstream dependencies:

- runtime source for copied observed agent runtime snapshots.
- scanner for session discovery from ACP and fallback sources.
- cloud reporter for session-only manager writes.

What to mock in `sessionreporter` tests:

- Mock observed runtime source with static runtime snapshots.
- Mock scanner behavior, including delays and failures.
- Mock cloud reporter behavior, including per-remote failures.
- Do not use real daemonstore, localapi, controlws, WebSockets, or local ACP processes.

Boundary rule:

- If a test asserts reporter scheduling, non-overlap, per-remote isolation, scan
  timeout, scan-to-report mapping, or best-effort error handling, it belongs here.
- If a test asserts supervisor desired-state reconciliation or slot lifecycle, it
  belongs in `supervisor`.
- If a test asserts ACP JSON-RPC behavior, it belongs in `acpclient`.
- If a test asserts cached local session list/sync behavior, it belongs in
  `localsessions`.

## Startup and scheduling

### Scenario: start returns immediately

Given the reporter is configured with a runtime source and scanner
When `Start` is called
Then it launches background work
And returns without waiting for scans or reports to finish

### Scenario: immediate run after start

Given one observed runtime has a cloud agent id
And the scanner returns one session
When the reporter starts
Then it scans without waiting for the first interval tick
And reports the session to that runtime's remote manager

### Scenario: ticker schedules later runs

Given a reporter is running
When the interval ticks
Then another scan/report pass is scheduled

## Runtime selection

### Scenario: observed runtimes are the source of truth

Given the observed runtime source returns two runtime snapshots
When a report pass runs
Then only those observed runtimes are scanned
And the reporter does not read desired agent connection state independently

### Scenario: runtime without cloud agent id is skipped

Given an observed runtime has no `CloudAgentID`
When a report pass runs
Then the runtime is skipped
And no report is sent for it

## Scanning

### Scenario: scanner receives one scanner spec per runtime

Given an observed runtime has command, working directory, environment, harness,
and agent type fields
When a report pass runs
Then the scanner receives those fields in a single scanner spec
And the fields are not passed as loose constructor parameters

### Scenario: per-agent scan timeout

Given a scanner blocks beyond the configured scan timeout
When a report pass runs
Then the scanner context is canceled
And the reporter continues with other agents

### Scenario: scan failure does not stop other agents

Given one agent scan fails
And another agent scan succeeds
When a report pass runs
Then the successful agent is still reported
And the failed scan is logged as best effort

## Reporting

### Scenario: report sessions per remote and agent

Given observed runtimes belong to different remotes
When a report pass runs
Then each agent's sessions are reported to its own remote manager
And sessions from one remote are never sent to another remote

### Scenario: report failure is non-fatal

Given a report to one remote fails
When a later run succeeds
Then the reporter continues running
And sends the later report

### Scenario: empty session list is not reported by default

Given a scan returns no sessions
When a report pass runs
Then no session-only HTTP write is sent for that agent

## Non-overlap

### Scenario: overlapping runs are skipped per remote

Given a remote's previous report pass is still active
When the next interval fires
Then that remote's next run is skipped
And another remote that is not active can still run
