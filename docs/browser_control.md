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
VNC sessions are random, bound to the authenticated remote source, capped at two,
expire after 30 idle seconds / 30 minutes total, and close on sequence mismatch
or buffer overflow. An ambiguous exchange must not be retried.

Pixels are sent in bounded 128 KiB chunks. Node queries are sequential; this MVP
adds latency compared with direct noVNC WebSocket and inherits Manager's node
connection routing constraints. VNC displays the complete shared Docker desktop.
Operator action metadata is logged without request payloads, pixels, password
values or VNC session credentials. Input bytes are not an individual-click audit.

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
