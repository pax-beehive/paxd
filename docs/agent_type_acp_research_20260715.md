# Agent type ACP research (2026-07-15)

This note summarizes the current ACP and local-integration status for new paxd
agent types. Here "ACP" means Agent Client Protocol, the editor/agent JSON-RPC
protocol used by paxd's local ACP process runner.

Important terminology: "ACP-compatible" does not mean "native ACP". Some agents
embed a first-party ACP server in their CLI, some rely on an adapter package,
some expose ACP over HTTP/SSE instead of stdio, and some have no ACP surface at
all. paxd should model that distinction because the implementation work is
different for each case.

## Executive summary

| Priority | Agent | ACP status | Likely paxd path |
| --- | --- | --- | --- |
| Existing | Codex | Adapter-based stdio ACP via `agentclientprotocol/codex-acp`; Codex CLI itself is not native ACP. The adapter starts the Codex App Server and translates ACP requests/events. | Keep using the ACP runtime path, but treat it as an adapter. Update stale package names to `@agentclientprotocol/codex-acp` where needed. |
| Existing | Claude Code | Adapter-based stdio ACP via `agentclientprotocol/claude-agent-acp`; Claude Code itself is not native ACP. The adapter uses the official Claude Agent SDK. | Keep using the ACP runtime path, but treat it as an adapter. Existing command/fallback shape is correct if it points at `claude-agent-acp` / `@agentclientprotocol/claude-agent-acp`. |
| P1 | OpenCode | Bundled/first-party stdio ACP command. Official docs say to run `opencode acp`; it communicates over JSON-RPC via stdio, and the GitHub repo has an ACP source tree. | Low-risk: add built-in harness and agentregistry entries with `Command: ["opencode", "acp"]`. Existing runtime can tunnel it. |
| P1 | OpenClaw | Gateway-backed stdio ACP bridge. Official docs say `openclaw acp` speaks ACP over stdio and forwards prompts to an OpenClaw Gateway over WebSocket. Not a full ACP-native editor runtime. | Medium: add a harness detector for `["openclaw", "acp"]`, keep existing Gateway discovery/session code, and expose gateway/session options carefully. |
| P1 | Pi Agent | Adapter-based stdio ACP via `svkozak/pi-acp`. The adapter speaks ACP JSON-RPC over stdio and spawns `pi --mode rpc`. | Low/medium: add a `pi` harness detector using `pi-acp` or `npx -y pi-acp`; keep local-log support for `~/.pi/agent/sessions`. |
| P2 | Grok Build | xAI news says Grok Build is a coding agent/CLI, supports headless `-p`, and provides full ACP support for bots and orchestration apps. Exact ACP subcommand/transport still not shown. | Medium: add after verifying `grok --help` / CLI docs. Expected implementation is a stdio command only if the CLI exposes one; otherwise wrap headless mode only. |
| P2 | Trae Agent | No ACP found. It is an open-source CLI agent with MCP server configuration and trajectory logs. | Medium/high: add a non-ACP adapter around `trae-cli` or implement/borrow an ACP shim. Not a direct tunnel candidate today. |
| P2 | Qwen Code | Has stable CLI ACP mode via `--acp`, plus `qwen serve` over HTTP+SSE and daemon ACP bridge docs. paxd already has Qwen local-log parsing. | Low for stdio: use `qwen --acp` / `npx -y @qwen-code/qwen-code --acp` in harness detection. Medium if adding `qwen serve` HTTP+SSE transport. |
| P2 | Z.ai / GLM / ZCode | GLM is a model family; Z.ai appears to have a coding product called ZCode, but I did not find official ACP docs. paxd already has an app-only `zcode` placeholder. | Medium/high: treat as app-only until official CLI/ACP docs are confirmed. If ZCode exposes ACP later, add stdio/HTTP detector. |

## What paxd already has

paxd already separates three concerns:

- Runtime tunnel: `runtime.AgentTunnelSession` starts a local ACP subprocess from
  `AgentConnectionSpec.Command`, then forwards JSON-RPC frames over the manager
  websocket tunnel. Any native server or adapter with a compatible stdio ACP
  command can work here.
- Harness discovery: `harnessregistry.DefaultDetectors()` currently includes
  Hermes, Codex, Claude Code, and Gemini only.
- Local agent registry/session sync: `agentregistry.Default()` already includes
  entries for `pi`, `qwen`, `zcode`, and `openclaw`, in addition to the older
  agents. Qwen has local log parsing; OpenClaw has gateway status/session list;
  ZCode is app-only; Pi has a third-party `pi-acp` adapter.

That means the fastest useful change is to expand `harnessregistry` defaults for
agents that expose stdio ACP, whether native or adapter-based, while preserving
the existing local session adapters for agents that are not stdio ACP.

## Existing ACP correction pass

### Codex

GitHub confirms Codex ACP is adapter-based, not native. The
`agentclientprotocol/codex-acp` repository describes itself as an ACP adapter
for Codex CLI and a stdio ACP agent server. It starts the Codex App Server,
translates ACP requests into Codex operations, and maps Codex events back into
the ACP client.

paxd support implications:

- Keep `codex` on the stdio ACP runtime path, because the adapter is exactly the
  shape `AgentTunnelSession` expects.
- Do not describe Codex as native ACP in docs or UI. Use wording like
  "ACP adapter".
- Check local registry/detector fallbacks. If paxd still references
  `@zed-industries/codex-acp`, update it to the current package name
  `@agentclientprotocol/codex-acp`.

### Claude Code

GitHub confirms Claude ACP is adapter-based, not native. The
`agentclientprotocol/claude-agent-acp` repository describes itself as an ACP
adapter for the Claude Agent SDK and says it implements an ACP agent by using the
official Claude Agent SDK.

paxd support implications:

- Keep `claude` / `claude-code` on the stdio ACP runtime path.
- Do not describe Claude Code as native ACP in docs or UI. Use wording like
  "ACP adapter over Claude Agent SDK".
- The current fallback package shape, `npx -y
  @agentclientprotocol/claude-agent-acp`, matches the GitHub adapter.

## P1 research

### OpenCode

Official docs say OpenCode is an open-source coding agent for terminal, desktop,
and IDE. Its ACP page says OpenCode supports Agent Client Protocol and should be
configured by running `opencode acp`; the command starts an ACP-compatible
subprocess over JSON-RPC via stdio. GitHub also has an ACP implementation under
`packages/opencode/src/acp`, so this appears to be bundled/first-party CLI
support rather than a separate external adapter. The docs say ACP mode supports
built-in tools, custom tools, slash commands, MCP servers, project rules from
`AGENTS.md`, formatters/linters, agents, and permissions, with a caveat that some
slash commands like `/undo` and `/redo` are unsupported.

paxd support needed:

- Add `opencode` to `harnessregistry.DefaultDetectors()`:
  - display name: `OpenCode`
  - capability: `acp`
  - command: `["opencode", "acp"]`
  - install hint: install from `curl -fsSL https://opencode.ai/install | bash`,
    npm package `opencode-ai`, or Homebrew.
- Add `opencode` to `agentregistry.Default()` if we want paxd local-session
  APIs, and therefore `paxl daemon local session ...`, to discover it without
  manual agent connection creation.
- No runtime protocol changes should be needed for tunnel mode.

### OpenClaw

Official docs describe OpenClaw as a self-hosted Gateway connecting chat apps to
AI coding agents. Its Gateway runbook documents `openclaw gateway status`,
`openclaw gateway status --json`, and `openclaw gateway status --require-rpc`.
It also documents a single multiplexed Gateway port serving WebSocket
control/RPC, OpenAI-compatible HTTP APIs (`/v1/models`, `/v1/chat/completions`,
`/v1/responses`, etc.), plugin HTTP routes, Control UI, and hooks.

OpenClaw also has official ACP CLI docs now. `openclaw acp` speaks ACP over
stdio for IDEs and forwards prompts to the Gateway over WebSocket. The docs are
explicit that this is a Gateway-backed ACP bridge, not a full ACP-native editor
runtime. Compatibility includes `initialize`, `newSession`, `prompt`, `cancel`,
`listSessions`, `resumeSession`, and `closeSession`; some areas are partial, and
per-session MCP servers plus ACP client filesystem/terminal methods are
unsupported.

The current paxd `agentregistry` implementation still aligns with the Gateway
model: `detectOpenClaw` probes `openclaw gateway status --json --require-rpc`,
and `listOpenClawSessions` shells out to
`openclaw sessions --all-agents --json`. `SteerSession` explicitly says gateway
injection is not implemented yet.

paxd support needed:

- Keep OpenClaw as `Kind: "gateway"` for registry/session discovery, but also
  add an ACP harness detector for `Command: ["openclaw", "acp"]`.
- For session list/status, the existing code is already close.
- For remote paxd tunnel support, the existing stdio ACP process runner should
  be enough for `openclaw acp`, provided a Gateway is reachable.
- Consider optional command args/env for `--url`, `--token-file`, `--session`,
  `--session-label`, and `--require-existing`.
- Avoid exposing unsupported ACP features as guaranteed. In particular,
  per-session MCP server forwarding, ACP client filesystem methods, and ACP
  client terminal methods are unsupported by the bridge docs.
- Add tests around current OpenClaw gateway status/session JSON contracts before
  relying on it as P1 production behavior.

### Pi Agent

I found a public ACP adapter for Pi: `svkozak/pi-acp`. The README describes
`pi-acp` as an ACP adapter for the Pi coding agent. It communicates ACP JSON-RPC
2.0 over stdio to an ACP client and spawns `pi --mode rpc`, bridging requests
and events between the two. It supports assistant streaming, tool call mapping,
session persistence, slash commands, and Zed session history. It stores ACP-to-Pi
session mapping under `~/.pi/pi-acp/session-map.json` while Pi stores its own
sessions under `~/.pi/agent/sessions/...`.

This matches existing local integration assumptions:

- `paxl/pkg/adaptor/pi_sessions.go` reads `~/.pi/agent/sessions` or
  `PI_CODING_AGENT_DIR`, and prompts via `pi --session <id> -p` or `pi -p`.
- `paxd/internal/agentregistry/registry.go` has a `pi` entry with
  `Command: ["pi-acp"]`, fallback `["npx", "-y", "pi-acp"]`, and install hint
  for `pi-acp` plus `@earendil-works/pi-coding-agent`.

paxd support needed:

- Add a harness detector for `pi`:
  - command: `["pi-acp"]`
  - fallback: `["npx", "-y", "pi-acp"]`
  - prerequisites: Node.js 22+, `pi` on `PATH`, Pi configured separately.
- Keep local session parsing for `~/.pi/agent/sessions` because that remains the
  source of Pi's persisted sessions.
- Optionally support `PI_ACP_ENABLE_EMBEDDED_CONTEXT=true` when paxd wants to
  advertise embedded context support through this adapter.
- Add tests with captured Pi JSONL logs before expanding session reporting.

## P2 research

### Grok Build

xAI docs and news now include "Grok Build", described as a powerful extensible
coding agent and CLI for professional software engineering. The install command
is `curl -fsSL https://x.ai/cli/install.sh | bash`, and basic commands include
`grok` and `grok -p "..."`. The Grok Build launch post says headless mode
(`-p`) works for scripts and automations, and that the CLI provides full ACP
support for bots and agent orchestration apps. The same docs say the underlying
model is available on the xAI API as `grok-4.5`; xAI model docs call Grok 4.5
their flagship for code and agentic tool calling. Earlier xAI news for
`grok-code-fast-1` describes it as an agentic coding model, available through
partner tools like GitHub Copilot, Cursor, Cline, Roo Code, Kilo Code, opencode,
and Windsurf.

The official pages confirm ACP conceptually but still do not show the exact ACP
command or whether the support is native, adapter-based, stdio, or HTTP. Do not
hardcode until verified.

paxd support needed:

- Verify CLI command:
  - `grok --help`
  - `grok acp --help` or whatever the docs/CLI expose
  - stdio JSON-RPC `initialize` smoke test.
- If stdio ACP exists, add a `grok` harness detector and agentregistry entry.
- If only headless exists, support one-shot tasks via `grok -p` separately from
  ACP tunnel sessions.
- Add env handling for `XAI_API_KEY`, since xAI docs document it for
  non-browser environments.

### Trae Agent

ByteDance's `bytedance/trae-agent` repository describes Trae Agent as an
LLM-based agent for general-purpose software engineering tasks. It provides a
CLI (`trae-cli run`, `trae-cli interactive`), supports multiple LLM providers,
file editing, bash, sequential thinking, trajectory recording, Docker mode, and
optional MCP services through `mcp_servers`.

I did not find ACP support in the official repo page. MCP support is not enough
for paxd's ACP tunnel, because MCP is a tool protocol consumed by the agent,
whereas paxd needs an agent protocol to drive sessions.

paxd support needed:

- Treat as non-ACP until upstream adds an ACP server.
- Possible integration paths:
  - a paxd-maintained ACP shim that runs `trae-cli` and maps prompts/results;
  - a local-log adapter using Trae trajectory files;
  - a one-shot executor integration for `trae-cli run`.
- This is not a drop-in `AgentConnectionSpec.Command` candidate today.

### Qwen Code / QwenCode

Qwen Code official README describes it as an open-source AI coding agent and
lists terminal UI, headless mode, IDE plugins, desktop, `qwen serve`, SDKs, and
IM bots. It says `qwen serve` is a shared agent session over HTTP+SSE (ACP),
and the docs include daemon ACP bridge and ACP-over-HTTP design pages. The
configuration docs also list `--acp` as stable Agent Client Protocol mode for
IDE/editor integrations like Zed, replacing the deprecated
`--experimental-acp` flag.

paxd already has `qwen` in `agentregistry.Default()` as local-log with
`~/.qwen/projects`, plus `QwenLocalElements`. This is useful for session
reporting even without live ACP.

paxd support needed:

- Short term: keep/finish local-log reporting from `~/.qwen/projects`.
- For live tunnel:
  - add a harness detector for stdio ACP with `qwen --acp`;
  - keep fallback `npx -y @qwen-code/qwen-code --acp`;
  - optionally add an HTTP+SSE ACP transport client for `qwen serve` later.
- If adding HTTP+SSE, this is a runtime transport extension, not just a harness
  detector, because current `AgentTunnelSession` assumes process stdio.

### Z.ai / GLM / ZCode

Z.ai is the company behind the GLM model family. The coding-related public
evidence I found is mostly model-centric: GLM-4.5 and GLM-5 papers emphasize
agentic, reasoning, and coding capabilities. News coverage references a Z.ai
coding product called ZCode, but I did not find official ACP/CLI docs during
this pass.

paxd already has a `zcode` app detector placeholder with possible app names
(`ZCode.app`, `Z.ai Code.app`, `Zai Code.app`) and explicitly marks it as
app-only with no ACP or local-log session adapter.

paxd support needed:

- Keep `zcode` as app-only until an official CLI/API/ACP surface is found.
- If ZCode exposes a stdio ACP command, add a harness detector.
- If it is desktop-only, integration likely needs app automation or an exported
  session/log format, which is outside the current ACP tunnel model.
- GLM models can still be used indirectly through OpenCode, Qwen Code, Trae, or
  other configurable agents that support OpenAI-compatible model providers.

## Suggested implementation order

1. Add OpenCode stdio ACP support.
   - This is the cleanest P1: no runtime transport changes.
   - Add harness detector, agentregistry entry, tests using testify-style asserts.
2. Add Pi adapter support.
   - Add `pi-acp` detector with `npx -y pi-acp` fallback.
   - Keep local Pi session parsing.
3. Add OpenClaw ACP bridge support.
   - Session discovery is already implemented.
   - Live tunnel can use `openclaw acp` when Gateway credentials/config are set.
4. Add Qwen Code stdio ACP.
   - Use stable `qwen --acp`; defer `qwen serve` HTTP+SSE unless needed.
5. Add Grok Build after verifying exact ACP CLI command.
   - Likely straightforward if its ACP is stdio.
6. Trae and ZCode.
   - Keep as adapter/shim candidates, not direct stdio ACP candidates.

## paxd implementation impact

`paxctl` has been migrated into `paxl` and is being retired. Treat
`cmd/paxctl` as legacy/reference code only. The active user-facing path is:

```text
paxl daemon ... -> paxl/internal/facade DaemonFacade -> paxd local API -> paxd control/store/runtime
```

The integration contract between repos is the paxd local API JSON schema. paxl
should not import paxd packages.

There are two different daemon-facing surfaces that should not be collapsed:

- Agent tunnel creation: `paxl daemon agent create --harness <name>` uses paxl's
  daemon facade to resolve a harness command through paxd local API, stores it
  as desired state in paxd, and `runtime.AgentTunnelSession` starts that process.
- Local session discovery/sync through paxd: `paxl daemon local session
  list|sync` calls paxd local-session APIs, which use `agentregistry`,
  `sessionreporter`, and local log scanners to find historical sessions.
- Local session discovery/sync through paxl native commands: `paxl session ...`
  uses paxl's own `pkg/adaptor` registry. This is separate from paxd
  `agentregistry` and already has adapters for Codex, Claude, Pi, Kiro, Hermes,
  and OpenClaw.

### Discover path

Files:

- `internal/harnessregistry/registry.go`
- `internal/harnessregistry/registry_test.go`
- optionally `internal/control/types.go` if we want richer metadata than the
  current `HarnessView`.
- paxl consumer path:
  - `paxl/internal/facade/daemon_client.go`
  - `paxl/internal/facade/daemon.go`
  - `paxl/cmd/paxl/daemon.go`
  - `paxl/internal/model/daemon.go`

Current behavior:

- `DefaultDetectors()` is the built-in inventory for `paxl daemon harness
  discover` and daemon background refresh.
- `CommandDetector` only checks whether `Command[0]` or `FallbackCommand[0]`
  exists on `PATH`; it does not run an ACP protocol smoke test.
- Results are cached in `harness_inventory` through `daemonstore.UpsertHarnesses`.
- paxl's `DaemonFacade.DiscoverHarnesses` is already a thin wrapper around
  `POST /v1/harnesses/discover`; no paxl-side per-agent branching is needed for
  basic discover.

Needed changes:

- Add detectors:
  - `opencode`: `["opencode", "acp"]`, source `first-party`.
  - `openclaw`: `["openclaw", "acp"]`, source `gateway-bridge`.
  - `pi`: `["pi-acp"]`, fallback `["npx", "-y", "pi-acp"]`, source
    `adapter`.
  - `qwen`: `["qwen", "--acp"]`, fallback
    `["npx", "-y", "@qwen-code/qwen-code", "--acp"]`, source `official`.
  - `grok`: only after confirming the real ACP command; for now keep out of
    defaults or add as documented missing with no command.
- Update Codex detector fallback from `@zed-industries/codex-acp` to
  `@agentclientprotocol/codex-acp`.
- Extend tests to assert the full default detector set and the expected fallback
  commands.

Optional but useful:

- Add an opt-in `--probe` protocol smoke test for stdio ACP. Today discover can
  report available if `npx` exists even when the package fails at runtime. A
  smoke probe would start the command and send `initialize`.
- Add metadata to `HarnessView` such as `transport=stdio`, `surface=adapter`,
  `surface=gateway-bridge`, and `requires_gateway=true`. Today this has to be
  squeezed into `Source` and `InstallHint`.

### Create path

Files:

- paxl active CLI:
  - `paxl/cmd/paxl/daemon.go`
  - `paxl/internal/facade/daemon.go`
  - `paxl/internal/facade/daemon_client.go`
  - `paxl/internal/model/daemon.go`
- paxd local API/control/storage:
  - `internal/control/types.go`
  - `internal/control/service.go`
  - `internal/daemonstore/repository.go`
  - `internal/localapi/handler.go`
  - `internal/localapi/openapi.go`
- related tests in `paxl/cmd/paxl/daemon_test.go`,
  `paxl/internal/facade/daemon_test.go`,
  `paxl/internal/facade/daemon_client_test.go`,
  `internal/control/service_test.go`, `internal/daemonstore/store_test.go`, and
  `internal/localapi/*_test.go`.

Current behavior:

- `paxl daemon agent create --harness <harness> --name <name>` calls
  `DaemonFacade.CreateAgent`.
- `DaemonFacade.CreateAgent` first uses an explicit `--command` when provided;
  otherwise it reads cached harness inventory, then calls
  `DiscoverHarnesses(ctx, false, []string{harness})` for the requested harness.
- The create flow registers a cloud agent with `agentType` defaulting to the
  harness id, then stores `DaemonCreateAgentConnectionCommand` /
  `CreateAgentConnectionCommand` with `AgentType`, `Harness`, `Command`,
  `WorkingDir`, `Env`, and desired state.
- Control/local API/storage are already generic; they do not need per-agent
  code if the command is a stdio ACP process.
- `paxl daemon agent create` already has `--agent-type`, repeated `--command`,
  `--cloud-agent-id`, `--instance-id`, and `--working-dir`.

Needed changes:

- For OpenCode, Pi, Qwen, and OpenClaw ACP bridge, create mostly works once
  harness discovery returns a valid command.
- Do not add new work to `cmd/paxctl`; if it remains temporarily, keep it
  compatible but do not make it the primary integration surface.
- Add paxl `--env KEY=VALUE` support for agent create/update. The model/facade
  already has `Env map[string]string`, but `paxl/cmd/paxl/daemon.go` does not
  expose an env flag yet.
- Keep using paxl's existing `--agent-type`, `--command`, and `--working-dir`
  overrides for agent-specific cases:
  - OpenClaw may need `OPENCLAW_GATEWAY_TOKEN` or `openclaw acp --token-file`.
  - Grok may need an auth/env setup once the ACP command is known.
  - Users may want `npx -y ...` explicitly even when a global binary is absent.
- For OpenClaw, consider first-class flags or docs for `--session`,
  `--session-label`, `--url`, and `--token-file`; these can initially be
  represented as command args rather than schema changes.

No required changes:

- `internal/control/service.go` and `internal/daemonstore/repository.go` do not
  need agent-specific branching for stdio ACP agents.
- `internal/localapi/routes.go` does not need new endpoints for basic support.
- `paxl/internal/facade/daemon_client.go` already covers the needed local API
  endpoints for list/discover/create/update/restart/delete.

### Runtime path

Files:

- `internal/runtime/agent_tunnel_session.go`
- `internal/runtime/persistent_acp_process.go`
- `internal/runtime/acp_initialize.go`
- `internal/daemon/runtime_supervisors.go`

Current behavior:

- `AgentTunnelSession` starts the configured command, wires ACP stdin/stdout to
  the remote websocket tunnel, and uses a persistent ACP process pool.
- This works for any command that speaks ACP JSON-RPC over stdio.

Needed changes by agent:

- OpenCode: no runtime changes.
- Pi via `pi-acp`: no runtime changes, assuming the adapter keeps stdout clean.
- Qwen via `qwen --acp`: no runtime changes.
- OpenClaw via `openclaw acp`: no runtime changes for the first version, because
  the CLI hides Gateway WebSocket behind stdio ACP.
- Grok Build: no runtime changes if the final ACP command is stdio; unknown
  until CLI is verified.
- Qwen `qwen serve` HTTP+SSE: runtime transport extension required if we choose
  that path instead of `qwen --acp`.
- Trae/ZCode: runtime changes or shims required only if we decide to build an
  ACP adapter ourselves.

Runtime risks to test:

- Adapters that print banners to stdout can corrupt JSON-RPC. Prefer env flags
  if upstream provides them, for example OpenClaw documents
  `OPENCLAW_HIDE_BANNER=1` and `OPENCLAW_SUPPRESS_NOTES=1` for clean ACP
  streams in dev/direct entrypoint scenarios.
- `npx` fallbacks may be slow or network-dependent. Good for create ergonomics,
  but less ideal for long-running supervised production connections.

### Local sessions and capsule injection

Files:

- `internal/agentregistry/registry.go`
- `internal/sessionreporter/scanner.go`
- `internal/localsessions/service.go`
- local log parsers such as `internal/agentregistry/qwen_local.go`
- paxd local API routes under `/v1/local/...`
- paxl daemon-local commands:
  - `paxl daemon local overview`
  - `paxl daemon local session list`
  - `paxl daemon local session sync`
- paxl native session/capsule path:
  - `paxl/pkg/adaptor/registry.go`
  - `paxl/internal/facade/session.go`
  - `paxl/internal/facade/capsule.go`

Current behavior:

- `agentregistry.Default()` already contains `pi`, `qwen`, `zcode`, and
  `openclaw`, but it is separate from `harnessregistry.DefaultDetectors()`.
- `agentregistry.ListSessions` supports local logs for Codex/Qwen, gateway
  session listing for OpenClaw, Hermes local merge, and generic ACP
  `session/list`.
- `SteerSession` can inject via ACP for `Kind: "acp"`, but currently rejects
  `Kind: "gateway"` OpenClaw.
- paxl's native `pkg/adaptor.NewDefaultRegistry()` already includes Codex,
  Claude, Pi, Kiro, Hermes, and OpenClaw. It currently does not include Qwen or
  OpenCode.
- paxl's OpenClaw adapter already uses `openclaw acp` and has ACP
  `initialize`, `session/list`, and `session/prompt` helpers. paxl's Pi adapter
  currently uses local Pi logs plus `pi --session <id> -p`, not `pi-acp`.

Needed changes:

- OpenCode: add to paxd `agentregistry.Default()` as `Kind: "acp"` with
  `["opencode", "acp"]` so `paxl daemon local session sync --agent opencode`
  can use generic ACP list/prompt if OpenCode supports those methods. Add a
  paxl `pkg/adaptor` adapter separately if native `paxl session ...` should
  support OpenCode.
- Pi: already present as `Kind: "acp"`; update install hint/source to reference
  `svkozak/pi-acp`, and add tests for detect/list behavior.
- Qwen: either keep `Kind: "local"` for rich log parsing and add a separate ACP
  command field later, or introduce a registry model that supports both
  `LocalCommand` and `ACPCommand`. Today a single `Command` forces a choice
  between local logs and live ACP.
- OpenClaw: keep `Kind: "gateway"` for gateway session sync, but update
  `steerCommand` to allow `openclaw acp` for injection if we want capsule
  injection to OpenClaw sessions. This likely needs command args for a target
  `--session`.
- Grok: add to `agentregistry` only after real command is known.
- Trae/ZCode: leave as non-ACP/app-only unless a log parser or adapter exists.

Important design cleanup:

- Unify or cross-feed `harnessregistry` and `agentregistry`. Today adding a new
  agent often means touching both, and it is easy for them to diverge. A shared
  built-in catalog could generate:
  - harness detector rows for create/live tunnel;
  - local session scanner rows for historical discovery;
  - install hints and aliases for CLI/UI.
- Also decide whether local session ownership belongs in paxd or paxl long
  term. Given paxctl retirement, paxl native `pkg/adaptor` may be the better
  home for rich local session parsing, while paxd keeps only the daemon-local
  API needed for fleet/runtime visibility.

### UI/API discovery surface

Files:

- `internal/localapi/openapi.go`
- `internal/control/types.go`
- consumer UI/manager code outside this paxd package, if any.

Basic support does not require endpoint changes. The existing endpoints already
cover:

- `GET /v1/harnesses`
- `POST /v1/harnesses/discover`
- `POST /v1/agent-connections`
- `GET /v1/local/sessions`
- `POST /v1/local/sessions/sync`

paxl already consumes those endpoints through `DaemonLocalAPIClient` and renders
them under `paxl daemon ...`.

Enhancements worth considering:

- Add `HarnessView.Requirements` or structured metadata so UI can show
  "requires OpenClaw Gateway", "uses npx adapter", "needs Node 22", or
  "requires auth".
- Add create-time validation that rejects missing required env/args for
  OpenClaw remote Gateway mode, instead of letting the supervisor fail later.
- Add command preview in create flow so users can see whether paxd will run
  global binary or `npx`.

### Agent-by-agent first implementation plan

| Agent | Discover | Create | Runtime | Local sessions / inject |
| --- | --- | --- | --- | --- |
| OpenCode | Add paxd `opencode` detector. | Works through `paxl daemon agent create` once discover returns command. | No change. | Add paxd `agentregistry` ACP entry if daemon-local sessions matter; add paxl `pkg/adaptor` adapter if native `paxl session` support matters. |
| Pi | Add paxd `pi` detector for `pi-acp`. | Works through `paxl daemon agent create`; document Node 22, `pi`, and adapter prereqs. | No change. | paxd already has `pi`; paxl already has Pi local-log adapter but not `pi-acp` adapter behavior. |
| OpenClaw | Add paxd `openclaw` ACP detector in addition to gateway status support. | Works through `paxl daemon agent create` if Gateway config/auth is already available; paxl needs `--env` or command args for auth ergonomics. | No change for `openclaw acp`. | paxd gateway list stays; paxl already has OpenClaw ACP adapter; paxd injection needs `steerCommand` support and session targeting. |
| Qwen | Add paxd `qwen --acp` detector. | Works through `paxl daemon agent create`. | No change for stdio ACP. | paxd local logs stay; add paxl `pkg/adaptor` Qwen adapter if native `paxl session` support matters. |
| Grok | Wait for exact ACP command. | Works through `paxl daemon agent create` once discover returns command. | No change if stdio. | Add after `session/list` behavior is verified. |
| Trae | No detector unless an ACP shim is added. | Not a direct tunnel candidate. | Needs shim/runtime if supporting live tunnel. | Possible trajectory log parser later. |
| ZCode/GLM | No detector yet. | Not a direct tunnel candidate. | Needs official CLI/ACP or app automation. | App-only until logs/ACP found. |

## Source links

- OpenCode homepage: https://opencode.ai/
- OpenCode ACP docs: https://opencode.ai/docs/acp
- OpenCode ACP source tree: https://github.com/anomalyco/opencode/tree/dev/packages/opencode/src/acp
- Codex ACP adapter GitHub: https://github.com/agentclientprotocol/codex-acp
- Claude ACP adapter GitHub: https://github.com/agentclientprotocol/claude-agent-acp
- OpenClaw homepage: https://openclaw.ai/
- OpenClaw docs overview: https://docs.openclaw.ai/
- OpenClaw Gateway runbook: https://docs.openclaw.ai/gateway
- OpenClaw ACP CLI docs: https://docs.openclaw.ai/cli/acp
- Pi ACP adapter GitHub: https://github.com/svkozak/pi-acp
- Qwen Code GitHub: https://github.com/QwenLM/qwen-code
- Qwen Code docs overview: https://qwenlm.github.io/qwen-code-docs/en/users/overview/
- Qwen Code daemon mode: https://qwenlm.github.io/qwen-code-docs/en/users/qwen-serve/
- Qwen Code ACP bridge docs: https://qwenlm.github.io/qwen-code-docs/en/developers/daemon/03-acp-bridge/
- Qwen Code configuration docs: https://qwenlm.github.io/qwen-code-docs/en/users/configuration/settings/
- xAI Grok Build docs: https://docs.x.ai/build/overview
- xAI Grok Build launch post: https://x.ai/news/grok-build-cli
- xAI model docs: https://docs.x.ai/developers/models
- xAI Grok Code Fast 1 announcement: https://x.ai/news/grok-code-fast-1
- Trae Agent GitHub: https://github.com/bytedance/trae-agent
- Z.ai / GLM-5 paper entry: https://arxiv.org/abs/2602.15763
- Z.ai / GLM-4.5 paper entry: https://arxiv.org/abs/2508.06471
