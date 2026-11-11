# Mobile operator control

`mobile.control` follows the existing browser node-query path. Manager checks
user and node ownership before forwarding a fixed operation to paxd. Local
control callers are already trusted node operators. Requests use the existing
authenticated node connection and do not enter durable command storage.

The query is `{type:"mobile.control",mobile_control:{operation,payload}}`; the
result is `mobile_control` inside the ordinary daemon query result. Payloads
are JSON objects, at most 16 KiB, with no unknown fields.

| Operation | Payload / result |
| --- | --- |
| `state` | `{}` -> `{available,mode:"screenshots-and-recordings",templates,sessions,remoteWebRTC:false}` |
| `templates` | `{}` -> `[{name,available}]` |
| `create` | `{template}` -> public runtime session |
| `start`, `stop`, `takeover`, `release` | `{sessionId}` -> public runtime session |
| `destroy` | `{sessionId}` -> `{destroyed:true}` |
| `screenshot` | `{sessionId}` -> `{mime:"image/png",data,width,height,frameId}` |
| `tap` | `{sessionId,frameId,x,y}` |
| `swipe` | `{sessionId,frameId,x1,y1,x2,y2,durationMs?}` |
| `text` | `{sessionId,frameId,text}`; printable ASCII only |
| `key` | `{sessionId,frameId,key}`; HOME/BACK/ENTER/APP_SWITCH/DELETE |
| `recording_start` | `{sessionId,kind?:"screen"\|"audio",durationSec?}` |
| `recording_stop` | `{sessionId,recordingId}` |
| `recording_list` | `{sessionId}` -> public recording metadata |
| `recording_read` | `{sessionId,recordingId,offset?,length?}` -> `{id,mime,data,offset,length,totalBytes,eof}` |

Input requires the runtime's human takeover and a fresh frame reference from
that exact device. The reference expires and is consumed before dispatch. Input
and lifecycle operations are never retried automatically. Start waits for boot;
it can occupy the sequential node query channel until completion or cancellation.
After an ambiguous timeout, refresh state rather than replaying the request.
The bridge caps reads and input at 15 seconds, start at 12 minutes, create at
2 minutes, stop/destroy at 90 seconds, and takeover/release/recording-stop at
60 seconds. Start exceeds the runtime's maximum 600-second boot timeout plus
cleanup. The upstream request context can cancel sooner. Custom node shutdown
settings can still outlast a control deadline; inspect state on the host rather
than retrying an uncertain operation automatically.

Recording reads use only the finalized artifact API and a validated byte range.
Each raw chunk is at most 256 KiB, returned as base64; no arbitrary path, URL,
range header or credential is accepted. Screenshot JSON is bounded to 4 MiB.
Requests and pixels are not logged. Operator source and operation names are
logged without payloads. Runtime error bodies and private paths are not forwarded.

## Node setup

Install and configure agent-mobile-runtime on the node, including its SDK and a
clean dedicated template. Start its authenticated loopback service separately.
paxd discovers the configuration from `AGENT_MOBILE_CONFIG`, otherwise
`$XDG_CONFIG_HOME/agent-mobile-runtime/config.json` (default
`~/.config/agent-mobile-runtime/config.json`). This is a node setting, never a
frontend field. The selected config supplies only the private data directory
and control port. The default data directory is
`$XDG_DATA_HOME/agent-mobile-runtime` or `~/.local/share/agent-mobile-runtime`,
and the default port is 7341. A relative config/data path is rejected. The
selected config must exist and be a valid object; a missing or invalid selection
cannot silently fall back to another installed runtime.

The bridge reads `DATA/secrets/admin-token` locally. The token must be a regular
0600 file under private directories. Symlink tokens and insecure modes are
rejected. Requests always target literal `127.0.0.1`, disable HTTP proxy lookup,
and never follow redirects. Session credentials, credential file paths and
private template paths are removed from returned data. No cookie or login link
is minted or forwarded to Pax.

This integration does not forward native emulator gRPC, ADB, `Rtc`, WebRTC SDP,
or data channels. Local WebRTC can be enabled separately at the runtime, but
Pax receives `remoteWebRTC:false` and `capabilities.realtimeMedia:false`.
The bridge is not a network relay or a malware sandbox.

## Verification and rollout

Use the installed compatible gvm toolchain before Go commands:

```sh
source "$HOME/.gvm/scripts/gvm"
gvm list
gvm use go1.26
GOWORK=off GOCACHE=/private/tmp/pax-mobile-go-cache go test ./internal/mobilecontrol ./internal/control ./internal/daemon
GOWORK=off GOCACHE=/private/tmp/pax-mobile-go-cache go build ./cmd/paxd
```

The tests use disposable loopback HTTP servers and private temporary files.
They verify operation/argument allowlists, source authorization, credential
redaction, token modes/symlinks, single-dispatch input, redirect denial and
bounded artifact ranges. The main checkout and active paxd are not modified by
this worktree; build and deploy it together with matching Manager and Console
changes. Restarting the active daemon can interrupt existing agent sessions.

For rendered Console acceptance, the opt-in `TestMobileHTTPHarness` serves the
real adapter on a private Unix socket, never a TCP operator port. Its parent
directory must be mode 0700; the socket is mode 0600. Always pass `-count=1`:
Go's test cache cannot stand in for a running network listener.

```sh
AGENT_MOBILE_CONFIG=/absolute/private/config.json \
PAX_MOBILE_HARNESS_SOCKET=/absolute/private/control.sock \
GOWORK=off GOCACHE=/private/tmp/pax-mobile-go-cache \
go test -count=1 -run '^TestMobileHTTPHarness$' -v -timeout 12m ./internal/mobilecontrol
```

The harness accepts only `POST /mobile` with `{operation,payload}` and a local
`POST /shutdown`. It exits after ten minutes and is not compiled into paxd.
