# Ephemeral harness authentication

`harnessauth.Manager` owns a short-lived **paxl** child; it does not execute vendor
login commands or parse vendor CLI output. Authentication, provider discovery and
native credential storage belong to `paxl harness auth`. No second token database
is created. `PAXD_PAXL_COMMAND` uses the same resolver as session discovery.

The credential context is the daemon's OS account and environment. Per-connection
credential/configuration environments are not selected by this API. Existing ACP
processes are not restarted or conversations interrupted after login.

Manager first queries `operation: capabilities`. paxd probes
`paxl harness auth capabilities --format json` without account access; protocol
version 1 is required before a start is dispatched. Legacy executors are rejected
with an update instruction, not used as a fallback.

## Control flow

1. `harness_auth.login` with `operation: start`, `harness`, optional `action`
   (`login` or `logout`), provider, method and secret_stdin creates an attempt.
   Start never accepts a secret. The command ACK only acknowledges the attempt.
2. paxd runs `paxl harness auth <action> --harness <name> --protocol jsonl`.
   Version 1 events describe native messages/notices, prompts and one final result.
3. Poll `harness_auth.status` with the session_id. A lost start ACK can instead
   be recovered by its command_id; this never launches a second child.
4. For `awaiting_input`, submit the exact session_id, current prompt_id and one
   nonempty code line using a new command ID. Select prompts accept option IDs;
   text/secret prompts accept their requested value. Device codes go in the
   provider's browser, not an unrelated input form.
5. Cancel with the session_id. Completion requires both a valid final paxl
   result and a successful process exit. `configured`/`unknown` retain their
   native metadata meaning; they do not become verified account/model access.

Requests and inputs bypass the durable command journal. Only input digests are
retained for deduplication, never submitted values. Input buffers are cleared
after writing. Events/challenges stay in bounded memory and are cleared on
termination. The lifetime is five minutes by default, independent of request or
control-WebSocket disconnection; a reconnect uses the same attempt. Daemon restart
loses ephemeral attempts and requires an explicit new start.

Attempts are scoped by authenticated control source, Manager-supplied actor_id
and harness. The Manager derives actor_id from its verified principal; browsers
cannot choose it. Only one credential mutation runs at a time. Repeated command
IDs return the same attempt; changed payloads conflict. Stale prompt IDs and
cross-owner handles are rejected. Cancellation first interrupts paxl so it can
reap its native descendants, then escalates to process cleanup.

## HTTP entry points

Local Unix HTTP remains a control transport:

- POST `/v1/harnesses/:harness/auth/login`, `X-Pax-Command-Id` header.
- GET `/v1/harnesses/:harness/auth/status?session_id=...`.
- `command_id` recovers a start ACK; `operation=methods` or `provider` select native
  metadata queries without creating a login process.

Console uses authenticated Manager routes:

- POST `/api/v1/user/:user_id/nodes/:node_id/harness/auth`.
- GET the same route with harness plus session_id/command_id, or native metadata
  selectors. Responses use `Cache-Control: no-store`.

The browser stores only an opaque handle/command ID and harness, scoped to user
and node. Authorization URLs, inputs, native output and credentials do not enter
browser persistence, query-string input fields or the command journal. Current
prompts/challenges are ephemeral UI data. Terminal-only native flows remain local
until the separately tracked PTY transport exists; they are not emulated as model
prompts or silently converted to API billing.

## Verification

Unit tests use temporary paxl fixtures for ownership, current-prompt validation,
deduplication, deadlines, cancellation and missing/failing final results. The
opt-in `TestHarnessBrowserAuthentication` in pax-manager starts the real Console,
Manager control WebSocket, daemon control service and paxl binary, with fixture
vendor CLI/SDK implementations. It checks reload/reconnect, dynamic prompts,
secret input, cancellation and absence of secrets from browser/SQLite persistence.
No real installation, account login or logout is part of these tests.
