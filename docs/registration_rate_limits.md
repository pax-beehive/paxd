# Registration rate-limit handling

Interactive login retries HTTP 429 responses from registration start and poll.
The cloud client returns a typed `RateLimitError`; the login flow owns waiting
and retrying. Other cloud requests, token-based setup and WebSocket reconnects
are unchanged.

- `Retry-After` accepts integer seconds or an HTTP date.
- A missing or malformed header uses a 10-second delay. Zero and past dates
  use a one-second minimum to avoid a tight retry loop.
- Each start or poll operation permits at most three retries (four requests).
- Delays over one minute are returned to the caller for manual retry, without
  shortening the server's delay. The error includes the requested wait.
- Waiting respects login cancellation. In-flight HTTP calls retain the existing
  30-second HTTP client timeout.
- A poll retry preserves the original registration ID and poll token. It does
  not create another registration or reopen the browser.
- Only explicit 429 responses trigger retries. Transport failures and other
  statuses return immediately because a failed response may follow a successful
  registration creation.
- Rate-limit errors and retry notices do not include response bodies or tokens.

## Verification

Use the installed Go 1.26 toolchain via gvm and disable the parent workspace when
checking an isolated worktree:

```sh
GOWORK=off go test -race -count=1 ./internal/cloud ./internal/remotelogin
GOWORK=off go test -count=1 ./...
GOWORK=off go build ./cmd/paxd
GOWORK=off go build ./cmd/paxctl
```

Tests exercise real HTTP start/poll 429 responses followed by approval, a single
browser open, HTTP 401/500 failures without retries, delay parsing (including
overflow), bounded retries, cancellation and preservation of poll credentials.
Sleeper injection avoids real retry delays in tests; no production traffic or
flood testing is involved. This change does not configure server-side rate limits.
