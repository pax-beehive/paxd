# Turn identity across ACP transport

For ordinary Manager ACP traffic, reliable envelope metadata carries `turn_id`
alongside `manager_session_id` and `native_session_id`. The native worker's
JSON-RPC payload and request IDs are unchanged.

The pool dispatch context carries this envelope identity into prompt admission.
The runtime projector uses it as its sole `TurnID`. A new execution requires a
new ID; reconnecting, replaying output, and resolving a permission do not create
a new execution. Older managers and local/E2EE callers without this metadata
retain the existing locally generated ID fallback.

Snapshot reports include `turn_id` and the compatibility alias
`turn_instance_id` with the same value. Reset commands using the old field name
continue to compare this same ID. No second execution identity is generated for
tagged prompts.

The router captures the output turn before completing the prompt lease. The
outbound journal therefore stores the original `turn_id`; replay never looks
up the session's newer turn. A bounded in-memory index of the last 1024 finished
prompt requests also attributes late terminal responses by request ID and slot
epoch. Old process notifications can use this retained identity. Permission
responses tagged for another turn do not consume the pending permission.

Existing message history and journal retention are unchanged. This does not add
a permanent execution archive or an exactly-once execution guarantee across
daemon restarts. Native notifications that carry only a session ID still rely
on the worker's ordering within a process: notifications must precede that
prompt's terminal response. A worker violating that contract cannot have its
old and new notifications distinguished from session ID alone.

Upgrade Manager and paxd for end-to-end identity. Mixed versions remain usable
through the compatibility fields, but do not provide unified business identity.
