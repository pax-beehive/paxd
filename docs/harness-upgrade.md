# Remote harness and ACP adapter upgrades

Authenticated node-control clients send the existing durable command envelope:

```json
{
  "command_id": "unique-request-id",
  "type": "harness.upgrade",
  "upgrade_harness": {
    "harness": "claude-code",
    "component": "acp",
    "version": "1.2.3",
    "connection_id": "selected-connection"
  }
}
```

Supported harness values are `claude-code`, `codex` and `pi`; component is
`cli` or `acp`. CLI upgrades omit connection_id and select the native launcher
on the daemon PATH. ACP upgrades require a running connection belonging to the requesting
remote. Its command, environment and working directory select the installation.
Remote input cannot supply executable paths, shell commands or package URLs.

Execution starts after the received ACK is delivered. Duplicate command IDs
reuse the durable result and are bound to their original remote and payload.
Progress stays in `received` with `result.phase`: inspecting, waiting_idle,
installing, restarting, verifying_runtime, verifying, or rolling_back. `applied`
means installation and all affected new process epochs are verified. Adapter
upgrades verify ACPAgent.Version; native upgrades verify Runtime.Version.
`result.harness` contains the installation result and ACP runtime capability
reports, keeping adapter and underlying runtime identities separate.

The coordinator excludes concurrent daemon restart/upgrade maintenance, blocks
new ACP work and waits up to two minutes for active work to finish. It invokes
paxl's machine-readable local upgrade interface; outdated paxl or unsupported
installation layouts fail with an actionable error. paxl currently supports
Unix npm global installations with direct symlink launchers. Native installers,
Homebrew, npx and custom launch wrappers remain unsupported.

All running connections using the selected launcher are restarted. Every
desired slot must complete initialization in a new process epoch and report
the target component version. Verification failure restores the old launcher and
restarts the old adapter. Adapter package dependencies are installed with the
adapter; a separate CLI upgrade installs the selected native package. Pi supports only
the scoped `@ccgv2/pi-acp` package, not the legacy unscoped package.

Managed direct `codex-acp` and `claude-agent-acp` launchers always receive an
absolute CODEX_PATH or CLAUDE_CODE_EXECUTABLE. Explicit nonempty overrides win;
otherwise the native launcher is resolved from the connection PATH and cwd.
Missing or invalid executables fail startup instead of selecting a bundled SDK
binary. This requires installing the native CLI alongside the adapter. Custom
wrappers and npx commands are not managed by this binding policy.

A native upgrade restarts all running managed connections using that launcher,
then checks every new process's runtime version. Connections belonging to a
different remote block the shared upgrade. Unknown custom launcher bindings
also block native upgrades. Native Pi updates remain independent: current Pi
ACP loads its own SDK, so only a Pi ACP update changes that SDK. External Pi SDK
binding through PI_ACP_SDK_ROOT will be integrated after the adapter supports it.

Version probes use the same binding as process startup. Model visibility also
depends on account/provider configuration and adapter compatibility. Upgrade
verification does not promise availability of a particular model; new sessions
obtain their model lists from the restarted runtime.

Tests use real command persistence, WebSocket transport, paxl execution, npm
staging and ACP process supervision. The opt-in `TestHarnessBrowserE2ENode`
fixture is launched by Manager's `TestHarnessBrowserUpgrade`; only packages
and the npm download step are fixtures. No developer installation is upgraded.
