# DSCODE integration

Use the official self-contained DSCODE terminal release. GitHub release archives
and the maintained Homebrew tap currently provide 0.7.54; legacy npm launcher and
bundle packages remain at 0.7.40 and are no longer the current terminal channel.

```sh
paxl daemon harness install dscode --dry-run
paxl daemon harness install dscode
```

The command runs on the CLI host, verifies the official platform archive through
the reviewed upstream installer and requires no system Node or npm. Add
`~/.local/bin` to the daemon's PATH. Existing foreign command links are refused.
Configure providers with `/login` in the TUI. New state lives under
`~/.local/share/dscode-state`; migrating old Hub/source data and credentials is
explicit. Follow the upstream [distribution and migration guide](https://github.com/qiz029/dscode/blob/main/docs/distribution.md).
Standalone updates use `dscode update` with all sessions/schedulers stopped;
Homebrew uses `brew upgrade dscode`. DSCODE is not yet supported by the remote
`harness.upgrade` coordinator.

```sh
paxl daemon harness discover dscode
paxl daemon agent create --harness dscode --name dscode
```

Discovery checks the daemon's PATH for `dscode` without starting or installing
it. The advertised command is `dscode acp`. Native DSCODE owns the transport,
preset composition, credentials, default model, process lifetime and installation
locks. Configure credentials and custom providers in the TUI before connecting.
Discovery availability does not verify those settings or ACP support.

The installed DSCODE command must be resolvable in the daemon's PATH. For a
source checkout, use an explicit connection command consisting of
`node /absolute/path/to/dscode/bin/dscode.mjs acp`, or put a `dscode` wrapper
for that checkout on PATH. An explicit `--model provider/id` can be appended
to a custom command.

Native ACP sessions use the `ask` permission preset: the ACP client answers
approval requests. First-install progress goes to stderr; stdout is reserved for
protocol frames. Resume does not replay history and cannot take over a session
locked by another running process. See the upstream
[ACP guide](https://github.com/qiz029/dscode/blob/main/docs/acp.md) for capabilities.

The local-session reporter uses `paxl session list/get` and forwards the
connection's `DSCODE_HOME` and `PAXL_DSCODE_SESSIONS_DIR` overrides, preserving
`dscode:` identities in local-log and ACP fallback paths. Source installations
need a log-directory override pointing at `<checkout>/.runtime/sessions`, or
at `<DSCODE_ACP_HOME>/sessions` when that source-launcher override is used.
DSH storage overrides are not forwarded for DSCODE.

Local paxl delivery uses DSCODE's session bridge to queue messages into a
running session. The durable-log reader supports Harness v0–v4 formats.

Verified against upstream main `6bd354e`: all six native ACP unit tests and
the real-runtime probe passed (handshake, custom provider, tool call, client
allow/reject, cancellation, reply, list, resume, close and stdin EOF). The probe
used an isolated home and a local model fixture, with no paid model requests.
