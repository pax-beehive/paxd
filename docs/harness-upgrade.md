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
`cli` or `acp`. Version is optional: omitted, blank or `latest` resolves the
npm latest version once through paxl dry-run, before draining active work.
Installation, runtime verification and the final result use that exact version.
Explicit versions remain supported; ranges and arbitrary tags are rejected.
CLI upgrades omit connection_id and select the native launcher
on the daemon PATH. ACP upgrades require a running connection belonging to the requesting
remote. Its command, environment and working directory select the installation.
Remote input cannot supply executable paths, shell commands or package URLs.

Execution starts after the received ACK is delivered. Duplicate command IDs
reuse the durable result and are bound to their original remote and payload.
Progress stays in `received` with `result.phase`: inspecting, resolving_version, waiting_idle,
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
also block native upgrades.

Managed direct `pi-acp` requires `@ccgv2/pi-acp` >=0.7.0 and Node >=24.
At each start, paxd resolves `pi` from the connection PATH and cwd, follows its
launcher into `@earendil-works/pi-coding-agent`, and passes that absolute package
directory as `PI_ACP_SDK_ROOT`. The adapter loads the SDK and its dependencies
from this installation. Missing packages and wrapper launchers fail explicitly.
A Pi CLI upgrade restarts connections following that launcher and verifies
`_meta.pax.runtime.version` on every new slot; failure restores the old launcher
and SDK processes. Updating the adapter alone preserves the selected SDK.

An explicit `PI_ACP_SDK_ROOT` (connection environment, then daemon environment)
must be an absolute SDK package directory. Empty or invalid values fail without
fallback. Such connections remain on their configured SDK and are not restarted
by an independent global CLI upgrade. The connection configuration is not
rewritten with a resolved version directory, so default bindings follow future
launcher upgrades and rollbacks. Upgrade old adapters before relying on binding;
older adapters may ignore the environment variable.

Version probes use the same binding as process startup. Model visibility also
depends on account/provider configuration and adapter compatibility. Upgrade
verification does not promise availability of a particular model; new sessions
obtain their model lists from the restarted runtime.

Tests use real command persistence, WebSocket transport, paxl execution, npm
staging and ACP process supervision. The opt-in `TestHarnessBrowserE2ENode`
fixture is launched by Manager's `TestHarnessBrowserUpgrade`; only packages
and the npm download step are fixtures. No developer installation is upgraded.

The optional `TestPiSDKPublishedAdapter` launches a real published Pi adapter
through the process runner against two fixture SDK installations, switches the
Pi launcher, then restores it. Set `PI_ACP_E2E_ENTRY` to the unpacked adapter's
absolute `dist/index.mjs` path (with dependencies installed) and run
`go test ./internal/runtime -run '^TestPiSDK' -count=1 -v` using Node 24 on PATH.
It checks that the adapter's initialize metadata follows the SDK on every start.
