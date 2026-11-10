# Browser operator control

`browser.control` is an authenticated transient node-control query. The service
forwards source identity and a fixed operation name to `internal/browsercontrol`.
Manager must check node ownership before issuing it. Local control callers are
already trusted local operators. Requests do not enter durable command storage.

Supported operations: state, policy, decide, revoke, secret, resume_sensitive,
view, vnc_open, vnc_exchange, vnc_close. No caller-supplied host, port, URL,
credential, command or arbitrary proxy route is accepted.

Native requests use loopback 7331 and the installed admin token selected by
`~/.config/agent-browser-native/data-dir`. RFB connects only to loopback 5900,
reads `~/.local/share/agent-browser/secrets/vnc-password`, performs RFB 3.8 VNC
password authentication locally, and presents a private authenticated channel.
VNC sessions are random, bound to the authenticated remote source, limited to one interactive controller,
expire after 30 idle seconds / 30 minutes total, and close on sequence mismatch
or buffer overflow. An ambiguous exchange must not be retried.

Pixels are sent in bounded 128 KiB chunks. Node queries are sequential; this MVP
adds latency compared with direct noVNC WebSocket and inherits Manager's node
connection routing constraints. VNC displays the complete shared Docker desktop.
Operator action metadata is logged without request payloads, pixels, password
values or VNC session credentials. Input bytes are not an individual-click audit.

Docker preview uses the existing `view` operation with
`{"source":"docker","action":{"type":"screenshot"}}`. It creates a short-lived,
shared, authenticated local RFB connection, requests only Raw full-frame pixels,
encodes a JPEG of at most 1280 pixels wide, and closes the connection. It creates
no interactive viewer ID and sends no input. Captures are concurrency-limited,
bounded to six seconds and eight megapixels, and never written to disk or history.
The frontend retains only the latest frame and retries failed captures. Deploy
this paxd before the preview-first Console; the existing Manager `view` forwarding
and Docker VNC runtime need no change. Interactive takeover still uses the existing
VNC exchange transport and does not add an agent broker pause or a WebSocket relay.

Build with the repository's Go 1.26 toolchain. Verification:

```
go test ./internal/browsercontrol ./internal/control ./internal/daemon
go build ./cmd/paxd
PAX_BROWSER_VNC_TEST=1 go test -v ./internal/browsercontrol
```

The last command requires an existing Docker desktop and does not print its
password. `PAX_BROWSER_VNC_HARNESS=1` enables a ninety-second loopback-only test
harness on 17432 for the frontend smoke test; it exposes only VNC operations.
It is not compiled into the daemon.

Deploy together with the matching browser runtime, Manager and Console changes.
See agent-browser-runtime `docs/pax-browser-mvp.md` for the full rollout and
native one-use password lifecycle. Restarting paxd interrupts MCP processes;
source changes alone do not update the running daemon.


Docker takeover text input uses `view` with
`{"source":"docker","action":{"type":"text","vncID":"...","seq":0,"text":"..."}}`.
It requires a live initialized VNC session owned by the authenticated source,
uses the same monotonically increasing sequence as `vnc_exchange`, and permits
at most 2000 Unicode characters. Replay or ambiguous execution retires that
session. Preview-only callers do not get an interactive ID. Text is transient;
it is not stored as an ACP event, command history, log payload or argv value.

paxd invokes only the fixed `agent-browser` Docker container's `desktop-text.cjs`
helper under `flock`, with JSON on stdin. It accepts no caller-supplied executable,
container name or shell syntax. The node user needs access to the Docker CLI and
engine. Standard macOS Docker installation paths are supported for LaunchAgents
without a shell PATH. The helper owns a temporary UTF-8 clipboard, checks window
focus and dispatches a single paste; acknowledgement means dispatch, not proof
that the remote application stored the value. Inspect the field after a failure
and do not replay it automatically. Runtime image and Console must be updated
with paxd; there is no fallback to simulated Unicode key presses.

A successful new interactive takeover retires the prior viewer connection.
Both text and RFB input from the retired ID are rejected. Ordinary preview
captures do not replace the controller.
