# Disposable node routing hints

paxd authenticates to Manager with its existing Node Key. `X-Pax-User-ID` only
helps the regional Worker find the right origin. It grants no access, and a
missing, corrupt or wrong-user value can be recovered using the Node Key.

The Worker verifies the key at a fixed regional Manager using the read-only node
identity endpoint, checks the authenticated owner's D1 assignment, and forwards
the original request once. The destination Manager still checks the Node Key
and normal resource permissions. Business failures are not retried across regions.

Successful HTTP (2xx) and WebSocket (101) responses carry the actual user ID.
paxd caches it under `~/.paxd/cache/node-routing/<sha256>.txt`. The filename is
scoped to the service origin and Node Key; the file contains only the user ID,
not the key. HTTP/WS schemes share the same origin scope. New files are mode
0600 and directories 0700; updates use an atomic rename. Unreadable files and
write failures do not stop normal authentication. Failure responses do not
overwrite or clear an existing hint.

The cache is disposable and does not need backup. Deleting it causes the next
successful request to relearn the hint. It is separate from the persistent Node
Key configuration, which remains required. There are no extra routing tickets,
secrets or expiry/refresh steps to manage.

The cloud HTTP client, node control WebSocket, agent tunnel, artifact metadata
and E2EE node requests share this behavior. Request bodies, retry policy,
sequence/ACK handling and reconnect behavior are unchanged. Custom injected
HTTP clients keep their existing transport and should wrap it in
`noderouting.Transport` if hint caching is wanted.

This change does not change the configured cloud URL or initial pairing and
installer flow. Existing regional Managers that omit the response header remain
compatible. Enabling a unified machine hostname requires both Managers and the
Worker to support discovery and requires a separate pre-pairing rollout.
