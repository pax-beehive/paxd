# Agent artifact publisher behavior

The artifact publisher owns agent-produced files after the synchronous MCP
acceptance boundary. Agent code never receives manager upload tickets, GCS
session URLs, offsets, retries, or completion state.

## Durable acceptance

### Scenario: a regular file is accepted

Given an agent selects a readable regular file

When `publish_artifact` is called

Then paxd copies the bytes into its protected artifact spool

And commits an `artifact_publish_jobs` row

And only then returns `accepted=true` with a publication ID.

### Scenario: the source changes after acceptance

Given paxd accepted a publication

When the original source is changed or deleted

Then the daemon-owned snapshot remains unchanged

And all later hashing and upload reads use the snapshot.

## Background publication

### Scenario: binds a publication to exactly one manager remote

Given an agent requests a new artifact publication

When exactly one enabled manager connection owns its cloud agent ID

Then paxd persists that remote ID in the job before returning acceptance.

If zero or multiple remotes expose the cloud agent ID, acceptance fails
explicitly without creating a durable snapshot. Legacy rows without a remote ID
are still resolved and bound by the worker before its first manager request.
Once bound, retries and daemon restarts resolve the exact persisted
remote-and-agent pair; another remote with the same agent ID cannot take over
the job.

### Scenario: a new file requires upload

Given an accepted snapshot and an enabled manager connection for its agent

When the worker runs

Then it idempotently registers the publication

And verifies the daemon-owned snapshot hash

And prepares the natural file identity with pax-manager

And initiates a GCS resumable session

And uploads aligned chunks

And calls the idempotent manager completion endpoint

And persists the job as `available`.

### Scenario: manager selects S3 presigned PUT

Given prepare returns protocol `s3_presigned_put`

When the worker uploads the accepted snapshot

Then it sends the entire snapshot in one PUT to the ticket URL

And sends every ticket header unchanged

And accepts any successful 2xx response without persisting the presigned URL.

If a write-once PUT returns HTTP 412, paxd treats it as an ambiguous prior
success and continues to manager completion. The manager's authoritative HEAD
validation decides whether the existing object matches the publication. Other
non-2xx responses still fail the upload and remain retryable.

If the transport fails, the retry error must not contain the presigned URL or
its query credentials. A zero-byte snapshot is sent with a fixed
`Content-Length: 0` body and without chunked transfer encoding. The default
client bounds connection, TLS handshake, and response-header phases without a
fixed overall upload deadline.

If a presigned PUT or legacy resumable bearer URL returns any redirect, paxd
does not contact the redirect target. It treats the 3xx response as an upload
failure, so an interactive login page or final 200 cannot mask the failure.

### Scenario: manager selects an unknown upload protocol

Given prepare returns an upload protocol paxd does not implement

When the worker dispatches the upload

Then it records an explicit unsupported-protocol error in `retry_wait`

And retains the daemon-owned snapshot for a later compatible retry.

For compatibility with managers deployed before protocol dispatch, an omitted
protocol is interpreted as `gcs_resumable`.

### Scenario: snapshot exceeds the S3 single PUT limit

Given the accepted snapshot is larger than 5 GiB

When the worker validates the daemon-owned snapshot

Then it reports permanent failure code `artifact_too_large` to pax-manager

And persists the job as `failed` without hashing or retrying the upload.

If the permanent-failure report cannot reach pax-manager, paxd records that
reporting error for diagnosis but still keeps the deterministic local outcome
as `failed`; it does not put the oversized snapshot back in `retry_wait`.

### Scenario: manager already has the file

Given pax-manager returns `available` from prepare

When the worker processes the job

Then paxd records the returned artifact ID

And skips GCS initiation and byte upload.

### Scenario: daemon restarts during upload

Given the job row contains a resumable session URL and a last observed offset

When a new worker instance processes the job

Then it queries GCS for the authoritative committed offset

And resumes from that offset rather than trusting a missed progress frame.

### Scenario: the resumable session expired

Given GCS returns HTTP 404 or 410 for the stored session URL

When the worker processes the job

Then paxd clears the resumable URL and offset

And leaves the job retryable

And a later pass requests a fresh manager ticket.

### Scenario: a transient dependency fails

Given manager or GCS is temporarily unavailable

When a background operation fails

Then paxd persists `retry_wait`

And retains the daemon-owned snapshot

And the agent is not asked to retry.

## Integrity and permanent failure

### Scenario: the owned snapshot disappears

Given MCP acceptance previously succeeded

When paxd can no longer open the daemon-owned snapshot

Then it reports `local_snapshot_unavailable` to pax-manager

And marks the local job `failed` even if that best-effort failure report cannot
be delivered, because retrying cannot restore a missing daemon-owned snapshot.

### Scenario: the owned snapshot hash changes

Given a hash was previously persisted for the daemon-owned snapshot

When a later worker pass calculates a different SHA-256

Then no bytes are uploaded

And paxd reports `local_snapshot_hash_mismatch`

And retains the spool for diagnosis.

## Security

- Resumable session URLs are bearer credentials stored only in protected local
  daemon state.
- Session URLs are never returned through MCP or logged.
- Presigned and resumable bearer URLs are never followed across redirects, and
  request/transport errors persisted by the worker omit their URL and query.
- Manager ownership comes from the authenticated node connection, not agent
  input.
- The worker uses the cloud agent and session identities injected into the MCP
  process by paxd.
- No automatic cleanup deletes snapshots or job rows in this version.
