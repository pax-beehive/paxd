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
