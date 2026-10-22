# Local file fetch from the web console

When a user clicks a local-file link in the console, pax-manager asks paxd to
check the path and then fetch it. paxd snapshots the file and runs it through
the existing artifact upload pipeline (register, hash, prepare, upload,
complete). Agent `publish_artifact` is unchanged; none of these rules apply to
it.

## Wire contract

Query `artifact_fetch.check` with `check_artifact_fetch: {path}` returns
`artifact_fetch_check`:

```json
{"allowed":true,"path":"/Users/me/proj/x.md","filename":"x.md","size_bytes":123}
{"allowed":false,"path":"/Users/me/.ssh/id_rsa","filename":"id_rsa","denial":{"code":"hidden_path_denied","rule":"builtin.hidden-path","message":"Hidden paths cannot be fetched."}}
```

Policy and file-state problems are `allowed:false`; `QueryResult.error` is only
for malformed requests (empty or over-long path), unauthenticated sources and
internal errors. The check never reads file content.

Command `artifact.fetch` with `fetch_artifact: {publication_id, agent_id,
session_id, path}`. `publication_id` matches `^apub_[0-9a-f]{32}$` and must
equal `command_id`. On success the ack is `ok:true, status:"applied",
target_type:"artifact_publication", target_id:<publication_id>` with
`result.artifact_fetch: {publication_id, status:"snapshotting", filename,
size_bytes}`. Denials are `ok:false, status:"rejected"` with
`error: {code:<denial code>, message, target:<rule id>}`. The command bypasses
the control command store; a repeated publication ID returns the existing
job's current status if agent, session and path match, otherwise `conflict`.

Both are remote-only. The agent is resolved through the artifact target
resolver and its remote must equal the requesting remote, otherwise
`unauthorized`.

## Rules (`internal/artifactfetch`)

| code | rule | when |
|---|---|---|
| invalid_path | builtin.home-root | not absolute, NUL/CR/LF, over 8192 bytes |
| outside_home | builtin.home-root | outside `$HOME` after Clean, or a symlink leaving the `os.Root` (absolute symlinks always count) |
| hidden_path_denied | builtin.hidden-path | a segment starts with `.`, except `.git`, `.claude`, `.codex`, `.agent`, `.agents` |
| credential_path_denied | builtin.credential-path | `.git/config`, `.codex/auth.json`, `.codex/config.toml`, `.claude/settings*.json`, `.claude/.credentials.json` |
| credential_path_denied | builtin.sensitive-file | key/keystore/password-db/SQLite extensions, SQLite `-wal`/`-shm`, `id_rsa*`-style names |
| not_found / permission_denied / not_regular_file | builtin.home-root | open or fstat outcome |
| owner_only_denied | builtin.owner-only | non-Windows, opened handle has no group/other bits; message suggests `chmod 644` |
| too_large | builtin.size-limit | over 5 GiB |
| secret_detected | builtin.secret-scan | asynchronous content scan hit |

Rules run in that order. Path rules are a pure table; stat rules use the fstat
of the handle opened through `os.Root`, with `O_NONBLOCK` on Unix so a FIFO
cannot block the node-control channel.

## Flow

The command handler validates, binds the remote, checks the rules, persists a
job (`origin=user_fetch`, `status=snapshotting`, planned spool path, stat size)
and acks immediately. The node-control channel carries one request at a time,
so copying a large file before the ack would block the node.

The worker registers the publication first, so a later snapshot failure can
always be reported through `Fail` (pax-manager rejects `Fail` for an unknown
row). It then re-opens the file through the home root, re-runs the stat rules,
copies it into the spool and continues the normal flow. Whether a snapshot is
still needed is decided by the spool file's existence, not by the job status,
because a transient failure moves the job to `retry_wait`. After hashing, user
fetch jobs enter `scanning`: the spool copy is streamed through overlapping
windows (binary files with a NUL in the first 8 KiB are skipped) looking for
private keys and AWS, GitHub, Slack, Anthropic, OpenAI, Google, Stripe and pax
(`pax_`/`paxu_` + 48 hex) keys. Any permanent failure is reported through the
manager `Fail` endpoint and the job's spool directory is removed.

## Security notes

- Rules are enforced on the node, which is the only party that sees the real
  filesystem. The manager and console only render paxd's answer, so a
  compromised console or manager cannot widen access.
- The scan runs before `prepare` and reads the exact bytes that would be
  uploaded, so nothing leaves the machine unscanned and a file swapped after
  the snapshot cannot bypass it. Findings report only rule and line, never the
  matched text.
- There is no web override, config file or CLI switch. A denied file can be
  made fetchable only by someone with local access (for example `chmod 644`,
  or moving it out of a hidden directory), which keeps the decision with the
  node owner.
- Agent publications keep their own trust model and are not scanned.
