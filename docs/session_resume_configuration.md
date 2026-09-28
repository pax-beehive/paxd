# Configuration after session resume

An ACP agent can return configOptions, legacy models, modes, and extension
metadata in a session/resume response. The router preserves the complete result
for explicit resume replies, including the path that first creates a missing
route. The reply uses the Manager caller's request ID.

Automatic cold resume uses an internal paxd request ID. After successful resume
and route binding, paxd emits a `_pax/session_resumed` notification through the
normal reliable output boundary before sending the pending session operation.
The notification has no request ID and carries:

```json
{
  "jsonrpc": "2.0",
  "method": "_pax/session_resumed",
  "params": {
    "sessionId": "native-session-id",
    "result": {"configOptions": []}
  }
}
```

The boundary maps the session ID to its Manager ID as usual. Manager consumes
the returned config/model state and stores it in the existing session config
snapshot. This is a PAX extension notification, not a new standard ACP method.
No notification is emitted when the result contains neither configOptions nor
legacy models. A failed resume does not publish a success snapshot or send the
pending prompt. Commands remain separate available_commands_update frames and
continue through the same output path.

Deploy a Manager version that understands this notification before deploying
paxd; older Managers ignore it and therefore do not refresh the config snapshot
on automatic resume. No migration is required. Console reads the existing
configuration endpoint and offers commands in the shared session composer.

Tests cover complete explicit responses, automatic config/legacy-model
notifications before prompt dispatch, command forwarding during resume, empty
results, and existing resume failure behavior. Manager separately verifies
notification identity normalization and reading both snapshots through HTTP.

## Suppressing resume transcript replay

`session/resume` restores an existing session; PAX already has its durable
transcript. Some agents nevertheless emit old `session/update` notifications
while restoring it. The router drops known transcript updates (user/assistant
text, thoughts, tool calls/updates, and plans) before they reach the Manager
output sink. This prevents these frames from being journaled and projected as
new messages under the pending turn.

The guard is scoped to native session, slot ID, and process epoch. It starts
before internal or explicit resume dispatch and stays active after the resume
response, including while `_pax/session_resumed` configuration is published.
It opens immediately before dispatching the next `session/prompt`, or an
explicit `session/load` that intentionally requests history. It does not open
for configuration changes or cancel. Failed resumes retain the guard; a failed
prompt/load send restores it. Slot removal/replacement clears the old scopes.
A resume requested during an already-active prompt does not mute that turn.

Resume results/errors, permissions and other worker RPC requests, commands,
usage, configuration updates, and unknown extension notifications are preserved.
The router does not filter by missing turn ID alone or compare message text.
There is no timer or added delay before dispatching the prompt.

This is a bounded compatibility defense, not a universal replay detector. An
agent that asynchronously continues unmarked replay after the new prompt has
been dispatched can interleave it with real output; ACP notifications then lack
sufficient identity to distinguish the two. The response/configuration marker
must not be treated as proof that such an agent finished replay. Fully handling
that case requires an agent-provided replay boundary or provenance. This change
does not repair existing Manager history rows or modify any worker installation.

Regression tests cover automatic cold restore, explicit restore of a missing
route and an already-hot route, replay before and after the resume response,
state/request preservation, fresh prompt/load delivery, failed send/resume,
cancellation, and scope cleanup on worker replacement. Deploy the updated paxd
on the node hosting the affected agent for this protection to take effect.
