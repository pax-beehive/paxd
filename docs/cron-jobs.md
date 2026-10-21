# Pax scheduled tasks

Pax owns the schedule. `paxd` stores jobs, runs, session bindings, transcripts,
and encrypted retry envelopes in its configured SQLite database (normally
`~/.paxd/paxd.db`). The daemon schedules and executes through its existing ACP
pool. `paxl cron` is a local Unix-socket management client.

## Create and manage a task

Select a registered, enabled connection from `paxl daemon agent list`.
The connection selects the configured harness and credentials. The saved
workspace pins its connection, Manager agent, remote, API URL, and absolute cwd.

```sh
paxl cron create --name 'Daily review' --connection CONNECTION_ID \
  --schedule '0 9 * * *' --timezone America/Los_Angeles \
  --cwd /path/to/project --prompt-file ./review-prompt.txt --session-mode reuse
paxl cron list
paxl cron get JOB_ID
paxl cron update --prompt-file ./new-prompt.txt JOB_ID
paxl cron pause JOB_ID
paxl cron resume JOB_ID
paxl cron run JOB_ID
paxl cron runs --limit 20 JOB_ID
paxl cron delete JOB_ID
```

Output is JSON. `--prompt-file -` reads stdin. Prompt text is stored immediately;
changes to the source file do not change the saved job. Defaults are `new` session
mode, UTC, the caller's current directory, and a one-hour timeout. `--disabled`
creates a paused job. Timeout accepts whole seconds from 1s to 24h. Custom socket:
`paxl cron --socket /path/to/paxd.sock list`.

- `new`: create one native session per execution.
- `reuse`: create one native session on first execution and persist its binding.
  Subsequent runs resume it. Changing its connection, directory, or mode requires
  `paxl cron update --reset-session ... JOB_ID`. Reset retains prior sessions.
  Target changes and resets are rejected while an execution is queued/running.

## Session creation and history

1. The scheduler durably queues a run containing the prompt and workspace snapshot.
2. The executor waits for a ready ACP slot, reserves a local session record, and
   calls `session/new` or resumes the saved native ID through the ACP router.
3. It calls the node-authenticated Manager endpoint
   `POST /api/v1/node/agents/:agent_id/sessions/register`. Manager verifies ownership,
   deduplicates native ID and registration ID, creates the ordinary encrypted Pax
   session, and returns `session_id`.
4. paxd saves the Pax/native mapping and sends the prompt. Each input/output frame
   is journaled before encrypted publication through the reliable transport.
5. The Console uses ordinary session history and shows `Cron: TASK_NAME` in the
   recent-session list and workbench. The tooltip identifies the cronjob and the
   creation run. `metadata.origin` is a creation snapshot; reused sessions retain
   their original creation run even after later triggers or job renames.

`cron runs` includes prompt, cwd, connection, mode, local session ID, native ID,
and Manager `session_id`. Manager registration is retried after outages; local
execution can finish before that ID is available. Transcript and ciphertext are
persisted, so later synchronization reuses the same encrypted record bytes.
Canonical messages/tools use the existing E2EE history format. Live output is
published only after its durable cursor, avoiding repeated streamed chunks.

Permission requests use the ordinary encrypted Console path and wait for a user
response. The scheduler does not grant permission. An unanswered request reaches
its run timeout. Agent settings and model usage are those of the configured ACP
connection. These changes do not install or launch a separate `paxl session start`.

## Scheduling and failure semantics

- Five-field expressions have minute resolution and IANA timezone/DST behavior.
- Four workers share the ACP pool. One job has at most one queued/running run.
  Overlapping scheduled occurrences are recorded as skipped; manual overlap is a
  conflict. A busy native session remains protected by the ordinary prompt lease.
- Downtime is coalesced into one catch-up run. Resuming a paused job starts at its
  next future occurrence. The daemon must be running; it does not wake a machine.
- Queueing snapshots the accepted task. Later edits apply to later runs. Pausing
  skips scheduled runs that have not started; deletion retains history. Already
  running tasks may finish. Paused jobs may be triggered manually.
- Queued work survives restart. Running work becomes interrupted and is never
  automatically retried: external actions cannot be guaranteed exactly once.
  Graceful shutdown/timeout sends ACP `session/cancel`; it does not kill a shared
  harness process. Agent process exit fails the local request.
- A crash or connection loss during session creation can leave an unknown native
  outcome. Reuse mode requires explicit binding reset before creating again.
- API patches require `expected_revision`; CLI fetches it before applying a patch.
- Run result summaries are capped at 256 KiB and errors at 8 KiB. Full local
  transcripts and retry envelopes are retained, including after job deletion.
  There is no automatic retention policy yet; monitor the SQLite database size.

Deploy the Manager registration endpoint before enabling tasks on updated paxd.
Update paxl for the management commands and Console for the origin badge. Existing
E2EE key provisioning remains required. No native-agent cron import, cron CRUD
Console page, distributed scheduler, or notification delivery is included.

## Local API

`/openapi.json` documents these endpoints:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/cron-status` | Scheduler status |
| GET / POST | `/v1/cron-jobs` | List / create |
| GET / PATCH / DELETE | `/v1/cron-jobs/{id}` | Read / update / delete |
| POST | `/v1/cron-jobs/{id}/run` | Queue a manual run |
| GET | `/v1/cron-runs?job_id=ID&limit=50` | Recent runs, maximum 200 |

Runs transition from queued to running and then succeeded, failed, interrupted,
or skipped. A successful manual trigger response means the run has been queued.
