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

And marks the local job `failed` only after the failure report succeeds.

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
- Manager ownership comes from the authenticated node connection, not agent
  input.
- The worker uses the cloud agent and session identities injected into the MCP
  process by paxd.
- No automatic cleanup deletes snapshots or job rows in this version.
