# DeepSeek Harness (DSH)

DSH is a native stdio ACP harness named `dsh`, launched as `dsh --profile acp`.
It uses the existing local-control discovery, cloud registration, connection
store, runtime slot pool, and ACP routing paths. No new daemon API is required.

## Install and create

Use Node 22.19 or newer on the 22.x line, or Node 24+. On the machine running paxd:

```sh
paxl daemon harness install dsh --dry-run
paxl daemon harness install dsh
paxl daemon harness discover dsh
paxl daemon agent create --harness dsh --name deepseek
paxl daemon agent list
```

The user-facing CLI lives in paxl; paxd owns discovery and runtime only.
Update both binaries for this integration. Installation explicitly runs
`npm install -g @deepseek-ai/dsh@latest` as the
invoking user, without sudo. Discovery never installs packages or starts DSH;
`available` means the executable is on the daemon's PATH, not that model
credentials or MCP connectivity have been verified. If several remotes exist,
add `--remote <remote-id>` to agent creation. Installation is local to the CLI
machine, not an installation request sent to a remote node.

Configure `DEEPSEEK_API_KEY` in the environment of the paxd service before
starting it, or use DSH's own credential configuration. An export in another
terminal does not change a running daemon's environment. Ensure that the daemon
can resolve both `dsh` and its Node interpreter. This integration does not
change user credentials, install DSH automatically, or restart existing agents.

## Runtime and MCP

The shipped DSH ACP profile supports `session/new`, `session/list`,
`session/resume`, `session/close`, model config options and cancellation.
Existing paxd cold-route recovery uses `session/resume`, not `session/load`.
Its list contains inactive resumable sessions, not complete live session inventory.
The scanner reports canonical `dsh:<native-id>` identities while retaining the
native ID for routing. DSH does not replay historical transcript over ACP.

With a paxl version containing the local DSH adapter, the scanner first reads
durable titles, settled messages and workspace roots through paxl. It forwards
the connection's `DSH_HOME` and `PAXL_DSH_SESSIONS_DIR` overrides and working
directory, without forwarding other connection-specific credential variables.
This path does not start DSH or load its credential file. Update paxl on the
daemon host as well as paxd; ACP fallback alone lacks titles and history.

Both new and resumed sessions accept stdio and Streamable HTTP `mcpServers`.
The existing post-decryption local MCP template injection remains unchanged:
`${PAX_AGENT_ID}`, `${PAX_SESSION_ID}` and `${PAX_SESSION_KEY}` are resolved
locally. Use absolute executable paths for stdio MCP servers and explicit env
entries for credentials/session keys; DSH scrubs credential-shaped ambient
environment variables. MCP startup/discovery failure rejects session admission.
Browser tab/profile isolation still belongs to the browser runtime.

DSH's ACP surface does not provide slash-command catalogs, modes, plans,
additional directories, MCP resources/prompts, or `session/load`. Do not infer
those capabilities merely because another harness supports them. npm ACP
`0.1.2-rc.1` was inspected to confirm MCP mounting; discovery is not a version
or handshake probe.

## Behavior-driven verification

- Given DSH is installed, discovery returns its native ACP command and caches it.
- Given only npx is installed, DSH remains missing with actionable setup guidance.
- Given an empty cache, agent creation discovers DSH, registers type `dsh`, and
  persists the discovered command through the existing control request.
- Given an explicit dry-run installation, print npm instructions without spawning npm.
- Given native ACP session summaries, preserve workspace and native ID while
  reporting a DSH-qualified identity.
- Existing runtime tests cover absolute workspace localization, per-session MCP
  injection, persisted resume descriptors, and cold-route session recovery.

Model-backed end-to-end acceptance requires a configured DSH installation and
DeepSeek credentials; unit/fake-process tests alone do not establish model access.
