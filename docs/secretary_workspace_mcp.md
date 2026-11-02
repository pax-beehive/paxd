# Secretary workspace tools through paxd

A Manager-created Secretary execution can use the existing `paxd mcp conversation
serve` stdio server for optional read-only workspace lookups. No user API key,
Python adapter installation, or `~/.paxd/mcp.json` entry is required. Manager
supplies `PAX_AGENT_ID`, `PAX_SESSION_ID`, and an execution-scoped
`PAX_SECRETARY_CAPABILITY` in that MCP child's launch environment.

Ordinary sessions retain their four existing conversation/artifact tools. A nonempty
capability environment switches discovery and dispatch to only these read-only
tools; `list_agents`, `ask`, `reply` and `publish_artifact` are then rejected even
when called directly:

| Tool | Input | Manager operation |
| --- | --- | --- |
| `workspace_search` | `query`, optional `kinds`, `session_ids`, `cursor`, `limit` | `search` |
| `workspace_read` | `ref`, optional `view`, `offset`, `limit`, `before`, `after` | `read` |
| `workspace_expand` | `message_id`, `content_hash`, `canonicalization_version`, `view`, optional `offset`, `limit` | `read` with the exact message ref |
| `workspace_history` | Target `session_id`, optional `turn_id`, `view`, `before_seq`, `after_seq`, `limit` | `history` |

Search/read/expand use the existing Manager search MCP schemas. Search is literal
text matching, with a maximum 256-character query, 50 results, 3 kinds and 50
session filters. Read supports `first`, `last`, and `full`; expand requires an
exact hash/canonicalization version and supports only `first` or `full`. Read
pages contain at most 16,384 characters and context counts are at most 50 each.
History accepts an optional `turn_id` to restrict the page to one turn's visible
messages, and `view` accepts `first`, `last` or `full`. With `view` omitted,
read/history use the Manager's compact role-aware default (full user text and
the assistant's `last` view), subject to response page limits. Use a turn filter
with `view: "full"` to inspect the turn's full visible text and follow pagination
as needed. Use `workspace_read` or `workspace_expand` with the returned exact
message ref and offset when a message has more text. Tool output and
reasoning remain excluded. History accepts at most one nonzero pagination
direction; its limit defaults to 20 when omitted or zero and has a maximum of 50.
It returns canonical visible
messages and pagination, excluding tools, reasoning and internal executions.
All advertised schemas reject unknown fields. Integer references and cursors are
parsed without floating-point conversion, including values above 2^53.

`workspace_read` preserves the existing ability to read a current source when its
version is omitted. An exact returned ref must be retained whenever a particular
version is intended. Expanding a message never drops its expected version.
Searching or reading does not publish summaries or advance Secretary checkpoints.
The Manager worker remains responsible for summary identity, validated coverage
and publication; there is no workspace publication or board tool here.

## Authentication and bridge

The helper resolves the local agent connection, its remote auth material, and the
existing node-key reference using the same credential resolver as conversation
MCP. It sends `X-Pax-Key` through the normal regional node transport. It does not
copy a credential into tool arguments or require a user credential.

```text
POST /api/v1/node/agents/:agent_id/secretary/tools
{
  "session_id": "<calling execution session from environment>",
  "capability": "<execution capability from environment>",
  "tool": "search | read | history",
  "arguments_json": "<validated tool arguments encoded as a JSON string>"
}
```

The outer caller identity is never taken from tool arguments. In particular,
history's target `session_id` remains inside `arguments_json` and cannot replace
the calling execution session. Manager must validate the node, selected agent,
active execution capability, owner and job/settings/lease fence on every call.
Having environment IDs, knowing a Secretary session prefix, or seeing the tools
in discovery does not itself grant permission.

The client forwards only the successful Manager envelope's `data`, unchanged,
as JSON text and MCP structured content. Exact IDs, versions and read receipts
are preserved. Requests/responses are bounded to 4 MiB; redirects are refused.
Caller cancellation reaches the HTTP request. Errors use fixed safe codes and
messages; node keys, capabilities, arbitrary backend bodies and resolver details
are never included in tool errors or logs. Do not put capability values in a
prompt, URL, committed configuration, or diagnostic output.

Older Managers without the bridge return a safe `unavailable` tool failure.
Without an injected capability, current paxd behaves as before. Older paxd does
not expose the new tools; lookup remains optional and a summary can still use the
context supplied in its prompt. Actual tool availability also depends on the ACP
adapter; OpenClaw's existing per-session MCP limitation remains unchanged. The
standalone user-key Python adapter continues to work independently.

The optional history `turn_id`/`view` arguments and role-aware compact defaults
require the corresponding Manager update in
[pax-manager PR 214](https://github.com/pax-beehive/pax-manager/pull/214).
Older Managers may reject the new optional fields; omit them until that server
update is deployed. The daemon reporting filter below can be deployed
independently and does not require these history arguments.

## Session reporting

Manager reserves `sess_secretary_` session IDs for internal execution work. The
daemon's periodic session reporter resolves native harness IDs through its
durable ACP route bindings before classifying them. It excludes these internal
Manager IDs both before `paxl session get` history reads and before metadata
uploads. A native ID, title or prompt containing the word Secretary is not a
classification signal. Internal routes remain available for the live execution
transport and its scoped tools.

The route-aware scanner lists native metadata without the report limit, removes
registered internal executions, then applies the configured report batch limit
before reading histories. This keeps a page of recent execution sessions from
hiding older user sessions while bounding history reads to the ordinary batch
size. Native metadata discovery retains the existing scan timeout; listing may
still read harness logs to derive metadata. The final upload guard also covers
alternate scanner implementations and ACP listing fallbacks.

This reporter uploads metadata only, not message bodies. Its existing route
mapping preserves the Manager execution ID rather than creating an ordinary
session alias. Manager's source/registry exclusions remain responsible for
keeping execution transcripts out of search and Secretary input. The daemon
filter supplements those exclusions and the normal session-list filter; it does
not remove local logs or historical Manager records. Deploy an updated paxd on
the selected agent's host to apply the reporting filter.

## Verification

BDD tests in `cmd/paxd/mcp_workspace_test.go` and
`internal/cloud/secretary_tools_test.go` cover ordinary/Secretary discovery,
closed-schema rejection, exact expansion, history/caller separation, large
integer preservation, bounded responses, old-Manager behavior, credential-safe
failures and HTTP cancellation. One stdio test exercises the real SQLite remote
and node-key resolver against a local HTTP server without a user API key.

`internal/sessionreporter/internal_sessions_test.go` covers internal metadata
suppression, classification through durable route IDs, ordinary native ID
preservation, unavailable route lookup and filtering before the history-read
batch limit. The reporter package has 84.5% statement coverage; the new
classification/filter helpers have 100% coverage.

```bash
GOWORK=off go test ./cmd/paxd ./internal/cloud -run 'TestWorkspaceMCP|TestSecretaryTool' -count=1
GOWORK=off go test ./...
```

Current verification: the complete serial suite (`go test -p 1 -count=1
-timeout=180s ./...`) passed all 53 tested packages with 1,492 test/subtest pass
events and 11 existing opt-in live tests skipped. New-file statement coverage is
91.3% for the MCP helper and 96.6% for the cloud bridge. `go vet ./...`, formatting,
binary build and actual binary stdio discovery/direct-call gating were checked.

An initial parallel run hit three unchanged load-sensitive fixtures (ACP login
initialization, runtime, updater executable smoke). Those packages passed in
isolation and both complete serial runs passed; no test deadline was changed.
Cross-service Manager acceptance is tracked separately; local helper tests use a
controlled HTTP server and do not call a live model.
