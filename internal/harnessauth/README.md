# Native harness login

This package runs the installed Claude or Codex login as a child process. The Claude adapter
passes a one-time authorization code to its stdin. Claude owns OAuth, token
refresh, and credential storage. paxd never reads or copies those credentials.
`claude auth status` verifies successful login and reports the current state.

The credential context is the OS account and environment running paxd (including
HOME and CLAUDE_CONFIG_DIR). Connections with a different account, config directory,
or credential environment may not use the resulting login. Existing Claude/ACP
processes may need to be restarted to pick up credentials; this API does not
interrupt conversations automatically.

## Local API

Use the Unix socket (default `~/.paxd/paxd.sock`):

1. POST `/v1/harnesses/claude/auth/login` with
   `{"harness":"claude","operation":"start"}` and an `X-Pax-Command-Id` header.
2. Read `result.harness_auth.session_id` from the command ACK.
3. Poll GET `/v1/harnesses/claude/auth/status?session_id=<id>` until
   `harness_auth.state` is `awaiting_code`. Open `authorization_url` in the
   user's browser. A local browser callback may also complete login directly.
4. POST the same login endpoint with
   `{"harness":"claude","operation":"submit","session_id":"<id>","code":"<authorization code>"}`
   using a new command ID. Submit the complete CLI authorization code, including
   its state suffix when present, not the account sign-in email verification code.
5. Poll until `succeeded`, `failed`, `expired`, or `cancelled`.

To cancel, POST `{"harness":"claude","operation":"cancel","session_id":"<id>"}`.
GET `/v1/harnesses/claude/auth/status` without a session ID returns the latest
login for the same source and harness, including a terminal result until its original deadline, or checks current credentials
and returns `logged_in` or `logged_out`. A submit without `session_id` targets the active login owned by
the caller, so CLI users do not need to manage session IDs.

`starting` and `exchanging` are asynchronous states. Successful command ACKs
mean the operation was accepted, not that the account has finished signing in.
Responses use `Cache-Control: no-store`. Do not log request bodies or authorization
URLs in callers or reverse proxies.

## Remote node-control protocol

The existing authenticated node-control WebSocket accepts the same typed operation:

```json
{
  "kind": "command",
  "command_id": "login-start-1",
  "command": {
    "command_id": "login-start-1",
    "type": "harness_auth.login",
    "harness_auth_login": {"harness": "claude", "operation": "start"}
  }
}
```

Poll using a query frame:

```json
{
  "kind": "query",
  "request_id": "login-poll-1",
  "query": {
    "type": "harness_auth.status",
    "harness_auth_status": {"harness": "claude", "session_id": "<id>"}
  }
}
```

Submit/cancel use `harness_auth.login` with the same payloads as the local API.
The ACK carries `command_ack.result.harness_auth`; query responses carry
`query_result.harness_auth`.

The paxl CLI exposes `auth status --harness claude` and `auth login --harness
claude`; use `login --code-stdin` to submit the code without a session argument. A
pax-manager user API and Pax Console login UI are not included. Manager integration
must check node ownership, avoid logging/persisting authorization codes, and
forward through the authenticated node-control channel. The daemon binds each
session to the local source or authenticated remote ID, not a browser user ID.

## Lifecycle and limits

- One active login process per daemon across both harnesses; an identical start from the same source returns that attempt. Changed methods or secret inputs conflict. A different source gets a conflict.
- Login state survives HTTP/WS request cancellation or reconnection, but not a
  daemon restart. On Unix, cancellation, expiry, and daemon shutdown stop the
  login process group, including descendants of Node or shell wrappers, and
  reap the login process.
- Login expires after five minutes, measured from process start; polling and repeated starts do not extend the deadline. Terminal results remain queryable until a
  later start/operation prunes expired entries. At most 32 sessions are retained.
- Recent results are retained per source and harness, independently of the
  single running mutation; starting Codex does not hide a failed Claude attempt.
- Retries with the same command ID and payload return the existing session state.
  Changed payloads conflict. Deduplication is in memory for the retained session;
  these commands do not create durable command records. Use session polling,
  not `/v1/commands/{id}`.
- Code input is single-line, bounded, and accepted once per login. Only a digest
  is retained for retry detection. Raw CLI output is discarded; public errors
  contain stable codes rather than process output.
- Only the supported native authorization hosts/paths are accepted. URL and prompt parsing is
  based on Claude Code 2.1.263; later CLI output changes may require adjustments.
- The CLI may open the target machine's browser as well as emit a remote link.

## Verification

Unit tests use temporary fake executables to cover process lifetime, split/ANSI
output, code submission, owner isolation, duplicate commands, failure, and timeout.
To check an installed Claude CLI without submitting a code, explicitly run:

```sh
PAXD_TEST_CLAUDE_BINARY=/absolute/path/to/claude go test ./internal/harnessauth -run '^TestRealClaudeLoginPipes$' -count=1 -v
```

This opt-in test starts and cancels a login and may open a browser.

## Codex and login methods

Both routes also accept `/v1/harnesses/codex/auth/login` and
`/v1/harnesses/codex/auth/status`. The body harness must match the route. The
shared `harness_auth.login` control command accepts optional `method`:

| Harness | Method | Native command | Input |
| --- | --- | --- | --- |
| Claude | `subscription` (default) | `claude auth login` | Browser callback or `submit` with authorization code |
| Claude | `console` | `claude auth login --console` | Browser callback or `submit` with authorization code |
| Codex | `device` (default) | `codex login --device-auth` | Browser entry of returned `user_code` |
| Codex | `api-key` | `codex login --with-api-key` | `code` field on start, passed only to stdin |
| Codex | `access-token` | `codex login --with-access-token` | `code` field on start, passed only to stdin |

Codex device login reports `awaiting_browser`, `authorization_url`, and
`user_code`; there is no submit operation. ANSI output and partial writes are
handled without publishing an incomplete code. Completion clears the challenge.
Status is parsed from the native command with bounded output; raw output,
including masked API keys, is never returned. Unrecognized output and command
failures remain errors rather than being treated as logged-out state.
Codex success also requires the reported credential method to match the attempt.
This does not establish credential identity or validate a model request.

`paxl auth cancel --harness <harness>` cancels the owned attempt without logging
out. paxl resolves an internal attempt handle before submitting or cancelling.
Native login defaults and flags were checked against installed CLI help and
[OpenAI authentication documentation](https://learn.chatgpt.com/docs/auth).
No provider login, credential pool, arbitrary credential context, or Console UI
is implied by this two-harness implementation. The wider model remains a proposal.

Integration tests require a freshly built `PAXL_TEST_BINARY`; they exercise
Claude code submission and Codex device login, cancellation/restart, and API-key
input through actual paxl processes and a Unix socket using fake native CLIs.
The real-Claude opt-in test only starts/cancels; none of these tests proves a
successful real-account OAuth flow. No real credentials are changed by the
regular unit or integration tests.
