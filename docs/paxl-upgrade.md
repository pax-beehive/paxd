# Paxl version reporting and remote upgrade

Paxd reports `heartbeat.paxl` and `status.get.status.paxl` from the executable it
uses for session discovery. Resolution honors `PAXD_PAXL_COMMAND`, then PATH,
then the sibling paxl binary. Symlinks resolve to a real path; command wrappers
are reported as `probe_failed` and cannot be upgraded remotely. Probing runs
`version --format json` with a three-second deadline and bounded output.

Observations contain `status` (`installed`, `missing`, `probe_failed`), `version`,
`commit`, `path`, `checked_at`, and an optional diagnostic `error`. An observation
is not a release lookup. Manager persists it as the node's last observation.

## Command

An authenticated node-control remote can send:

```json
{
  "command_id": "unique-client-command-id",
  "type": "paxl.upgrade",
  "upgrade_paxl": { "version": "1.2.3", "tag": "stable" }
}
```

The existing command store persists received before the ACK. Work starts only
after the WebSocket ACK is written. Same-ID/same-payload retries reuse the
record and are bound to its original remote. Query `command.get` for
`received`/`applied`/`failed`, `result.phase`, `result.paxl`, and errors. Received
phases are downloading, activating and verifying; applied has phase verified.

The installer resolves the issuing remote's artifact API, requires the exact
target version, verifies checksum and size, tests the staged executable,
preserves the previous binary and atomically renames within the installation
directory. It then probes the actual entry point before marking applied. Failed
post-activation verification restores the previous executable. Daemon identity,
paxl configuration and login state are untouched; paxd is not restarted.

`PAXD_PAXL_UPDATE_RESOLVER_URL` is a local configuration override for isolated
fixtures or self-hosted release sources. It is never accepted in a remote
command. The normal remote URL retains regional/self-hosted origin handling.

## Lifecycle and limits

New paxl invocations use the new binary. Existing processes retain their old
executable. Updated local `paxl update` and paxd share a nonblocking advisory
lock at `<real executable>.update.lock`. Older paxl releases do not honor that
lock; avoid concurrent legacy self-update during rollout. The installer also
checks for an intervening executable replacement before activation.

Known Homebrew, Nix, asdf and mise managed paths are rejected rather than
modified behind their package manager. Write permission errors fail the command.
Windows remote updates are unsupported. A lost connection does not cancel an
accepted upgrade. After a daemon interruption, retrying the same received
command rechecks the entry point; an already-installed target is verified
without reapplying the download. The configured resolver must return the
explicit target, so a moving stable tag can cause a safe version-mismatch failure.

## Tests

`internal/paxlinstall` exercises fixture upgrades, checksum/download/version
failures, rollback, concurrent writers, path resolution and managed installs.
`internal/control` exercises ACK ordering, deduplication, source binding and
received-command recovery. The opt-in `TestPaxlBrowserE2ENode` fixture in
`internal/controlws` is launched by Manager's `TestPaxlBrowserUpgrade`, using the
production WebSocket runner, SQLite store and installer with a real Console.
