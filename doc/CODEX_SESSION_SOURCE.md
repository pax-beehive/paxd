# Codex session source

paxl owns Codex session discovery, index/rollout parsing, compaction/resume
lineage, internal-thread filtering, and history normalization. paxd only resolves
the paxl executable, invokes its JSONL CLI, converts the wire records and reports
sessions to Manager. Native Codex execution and ACP routing remain in paxd.

Both the reporter and the registry/legacy CLI use paxl. A Codex read failure is
returned as an error; it must not invoke ACP or parse Codex files as a fallback.
Existing Manager metadata remains until the next successful report. Storage-only
`CODEX_HOME` overrides are forwarded for configured Codex connections.

History consumers preserve messages, reasoning, tool calls/results, durations,
usage and normalized fields. Snapshot headers are not history elements. paxl's
JSONL timeline adds `normalized`, `usageJson` and `raw` fields so this transport
boundary does not require a second transcript parser.

Deploy the updated paxl before the updated paxd. The matching source builds
preserve root session IDs, choose the latest non-internal lineage title, and read
the latest non-internal rollout after compaction. Explicit reads of an internal
thread still return that thread's own history.

Verification:

- paxl adapter/facade/CLI tests cover lineage and rich history.
- paxd registry/client/reporter tests cover delegation and failure propagation.
- `PAXD_TEST_PAXL=/path/to/paxl go test ./internal/agentregistry -run TestCodexPaxlIntegration`
  exercises the actual paxd-to-paxl executable boundary with isolated fixtures.
