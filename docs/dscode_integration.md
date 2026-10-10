# DSCODE integration

Use a DSCODE build containing native `dscode acp` support, merged into upstream
main at `6bd354e` (PR #7). The package version at that commit still says 0.7.35;
the earlier published 0.7.35 build does not contain this command. A version
string alone is therefore not sufficient to establish ACP support. Use a source
build containing this commit or a subsequent release that includes it.

```sh
paxl daemon harness discover dscode
paxl daemon agent create --harness dscode --name dscode
```

Discovery checks the daemon's PATH for `dscode` without starting or installing
it. The advertised command is `dscode acp`. Native DSCODE owns the transport,
preset composition, credentials, default model, process lifetime and installation
locks. Configure credentials and custom providers in the TUI before connecting.
Discovery availability does not verify those settings or ACP support.

Node 22.19+ on 22.x or Node 24+ and DSCODE must be resolvable in the daemon's
PATH. For a source checkout, use an explicit connection command consisting of
`node /absolute/path/to/dscode/bin/dscode.mjs acp`, or put a `dscode` wrapper
for that checkout on PATH. An explicit `--model provider/id` can be appended
to a custom command.

Native ACP sessions use the `ask` permission preset: the ACP client answers
approval requests. Title, session-card and memory generation are disabled in
this transport. First-install progress goes to stderr; stdout is reserved for
protocol frames. Resume does not replay history and cannot take over a session
locked by another running process. See the upstream
[ACP guide](https://github.com/qiz029/dscode/blob/6bd354e/docs/acp.md) for capabilities.

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
