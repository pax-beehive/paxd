# Harness authentication model proposal

Status: proposed replacement for the current Claude/Codex protocol; the general
multi-provider and multi-credential model is not implemented.

Deferred implementation: [KEV-92](https://linear.app/kevin-geng/issue/KEV-92).

## Ownership and scope

The harness remains the source of truth for credentials, refresh, pool order,
and provider selection. Pax exposes an adapter view and performs supported
native operations; it does not create a second credential database.

Every request resolves an AuthScope: node, harness, and credential context.
The default context is the daemon's OS account and harness configuration.
A connection using a different configuration or credential environment needs
an explicit context. Context IDs resolve to daemon-controlled configuration;
clients cannot supply arbitrary filesystem paths, environment, or executables.
Multiple connections sharing a credential store share its mutation lock.

Provider identifies the service being authenticated, not the harness. An
optional account identifies the upstream user or organization when the native
harness reports it. Neither provider nor email uniquely identifies a credential.
One provider can have multiple credentials, including multiple credentials for
the same account. A subscription versus Console choice can be a method on the
same provider; provider identifiers belong to the adapter, not a global guess.

## Read model

AuthSnapshot {
  scope: { harness, context_id }
  observed_at
  providers: ProviderAuth[]
  pending_login?: LoginAttempt
  error?: { code, message }
}

ProviderAuth {
  id, label
  methods: AuthMethod[]
  enumeration: complete | partial | unavailable
  credentials: CredentialView[]
  selection: {
    mode: single | ordered_pool | dynamic | unknown
    credential_ids?: string[]
    observed_effective_id?: string
  }
}

AuthMethod {
  id, label
  interaction: browser_code | browser_callback | device_code | secret_input
  result_kind: oauth | api_key | token | ambient | unknown
  intents: (establish | add | replace)[]
  effect: create | replace_default | upsert | adapter_defined
}

Interaction describes how the user participates, not what gets stored. For
example, a browser flow can create an API key. Method IDs and capabilities
are discovered for the installed harness version and enabled providers.
Unsupported operations are explicit, never emulated by overwriting files.

CredentialView {
  id?: string
  label?: string
  kind: oauth | api_key | token | ambient | unknown
  source: native_store | environment | helper | cloud_identity | unknown
  account?: { id?, display_name?, organization? }
  state: configured | expired | revoked | missing | unknown
  expires_at?: timestamp
  check?: {
    result: valid | invalid | unavailable | unknown
    checked_at: timestamp
    basis: local_metadata | provider_request
    error_code?: string
  }
  availability: ready | cooldown | disabled | unknown
  cooldown_until?: timestamp
  capabilities: { replace: bool }
}

Credential IDs are opaque adapter references to native IDs, never array
positions, email addresses, key hashes, or secrets. If the native tool cannot
identify entries stably, omit the ID and disable targeted replacement. Empty
credentials with enumeration=unavailable must not be presented as logged out.
Configured does not imply remotely verified. Cooldown or quota exhaustion is
not an authentication failure. Pool order and the currently observed selection
are distinct; dynamic routing need not have one active credential.

Status is a metadata read by default. Any live provider probe must be a
separate explicit capability and operation; errors such as network failure or
rate limiting must not be interpreted as invalid credentials.

## Write model

StartLogin {
  scope
  provider?: string
  method?: string
  intent: establish | add | replace
  credential_id?: string
  label?: string
  command_id: string
}

- establish means the native default sign-in behavior, with its effect exposed
  by AuthMethod. Use it only where the native behavior is unambiguous.
- add explicitly creates another stored credential. Never infer replacement
  from a matching provider or account.
- replace requires an existing stable credential ID and adapter support. It
  must not silently append to the pool. If the native tool cannot update that
  entry, return unsupported before starting authorization.
- provider/method can be omitted only when resolution is unambiguous. Otherwise
  return choices without spawning a process. Interactive clients can render
  the choices; scripts pass flags. Never silently choose a provider based on
  list ordering or a possibly transient current routing decision.

ContinueLogin {
  scope
  attempt_token: string
  challenge_id: string
  encrypted_input: SecretChannelEnvelope
  command_id: string
}

Secret input types include authorization_code, redirect_url, api_key, and token.
The adapter declares the expected type and validates it. Browser clients use
the existing encrypted secret channel. Local CLI stdin may use the protected
Unix socket; secret values must never appear in argv, logs, persistent command
journals, status snapshots, or general UI query/mutation caches.

LoginAttempt {
  attempt_token: string
  provider_id, method_id, intent
  target_credential_id?: string
  state: starting | waiting_user | exchanging | succeeded | failed | expired | cancelled
  started_at, expires_at
  challenge?: {
    id
    kind: browser_code | browser_callback | device_code | secret_input
    url?: string
    user_code?: string
    input_kind?: authorization_code | redirect_url | api_key | token
  }
  result?: { credential_id?: string }
  error?: { code, message }
}

Success means the native operation completed and its documented persistence
result was confirmed. It does not claim that a model request succeeded. Refresh
AuthSnapshot after completion, preserving any uncertainty about native entry
identity rather than attributing success to a pre-existing credential.

## Simple lifecycle without public session CRUD

Start with at most one active authentication mutation per credential store,
including all providers in that store. This conservative lock supports multiple
persisted credentials without requiring concurrent interactive login processes.

An identical start from the same authenticated owner returns the same attempt
and deadline. A different target, method, or intent conflicts. Another owner
cannot view or submit to the pending attempt. The remote owner binding must
include an authenticated user identity, not merely the manager connection ID;
clients cannot claim a user identity in an arbitrary request field.

The attempt token and challenge ID are internal protocol guards, not CLI session
management. They stop a delayed response from being submitted to a newer login.
A fresh paxl invocation resolves the current owned attempt and submits against
that exact token. Frontends retain the token from the displayed challenge. An
explicit old token is never retargeted; native OAuth state validation remains
necessary for a pasted code from an old flow.

paxd owns the process and stdin/PTY as required by the adapter. The deadline is
five minutes or the provider's earlier expiry. Polling and repeated starts do
not extend it. Client disconnect does not cancel it; shutdown or expiry kills
and reaps the process. Terminal outcomes have bounded retention for polling.
No public session list/create/update/delete commands are required. Credential
removal, activation, and pool-priority editing are outside this change.

## Proposed CLI

paxl auth status --harness claude
paxl auth status --harness hermes --provider anthropic
paxl auth login --harness claude --method console
paxl auth login --harness hermes --provider anthropic --method oauth --add
paxl auth login --harness hermes --credential <id> --method oauth
paxl auth login --harness codex --method api-key --secret-stdin
paxl auth login --harness claude --code-stdin

--credential implies targeted replacement; --add and --credential conflict.
The API uses typed intent values. Provider can be inferred from a stable
credential target. Missing required choices are interactive prompts only on a
TTY; machine callers receive a structured selection_required result.
--secret-stdin can establish a secret-input attempt and complete it within one
CLI invocation. --code-stdin continues the current owned browser-code attempt.
No --session flag and no local CLI daemon are introduced.

## Migration and acceptance

Split the current HarnessAuthView into AuthSnapshot and LoginAttempt. Remove the
combined logged_in/starting state enum. Generalize the adapter interface into
Discover, Snapshot, Start, Continue, and internal cleanup operations. Keep raw
process output and provider-specific settings behind adapters.

Acceptance cases:
- Two credentials for one Hermes provider are visible independently.
- Adding one credential leaves existing pool entries and priorities intact.
- Unsupported targeted replacement fails before changing anything.
- Browser authorization yielding an API key is represented correctly.
- Device authorization completes without a submitted code.
- An already configured credential cannot prove a new attempt succeeded.
- Missing enumeration and failed network probes are not reported as logout.
- Two frontend users and stale challenges cannot cross-submit.
- Timeout, daemon restart, and separate paxl invocations preserve ownership rules.
- Encrypted input reaches the native consumer without entering durable records.
- Different connections sharing one credential store cannot race mutations.

This proposal does not claim multi-harness implementation or end-to-end
verification. The current implementation supports Claude subscription/Console and Codex
device/API-key/access-token login, but not the general model above.
