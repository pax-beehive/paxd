# Pax Conversation MCP Design

This document records the target design for Pax agent-to-agent conversation MCP.
The current one-shot CLI commands are useful for smoke tests, but the MCP shape
should be session-stable: register the server once at `session/new`, keep the
session identity fixed, and avoid per-invocation environment updates.

## Session Identity

The MCP server should be started with stable session identity only:

```text
PAX_AGENT_ID=agent_runtime_x
PAX_REPRESENTATIVE_AGENT_ID=rep_runtime_x
PAX_SESSION_ID=sess_x
```

Do not use `PAX_INVOCATION_ID` as part of the target MCP server design. An MCP
server may live for the whole session, and ACP `session/update` cannot be relied
on to update already-registered tools or process environment.

## Registration

Register one conversation MCP server at `session/new`. It exposes both `ask` and
`reply` tools.

```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "session/new",
  "params": {
    "cwd": "/Users/gengcongkai/pax_workspace",
    "env": {
      "PAX_AGENT_ID": "agent_runtime_x",
      "PAX_REPRESENTATIVE_AGENT_ID": "rep_runtime_x",
      "PAX_SESSION_ID": "sess_x"
    },
    "mcpServers": [
      {
        "name": "pax-conversation",
        "command": "/usr/local/bin/paxd",
        "args": ["mcp", "conversation", "serve"],
        "env": [
          { "name": "PAX_AGENT_ID", "value": "agent_runtime_x" },
          { "name": "PAX_REPRESENTATIVE_AGENT_ID", "value": "rep_runtime_x" },
          { "name": "PAX_SESSION_ID", "value": "sess_x" }
        ]
      }
    ]
  }
}
```

## Ask Tool

`ask` starts an invocation to another representative agent. On success, it returns a
receipt token for audit/status lookup. The receipt token is not required for the
target agent to reply.

Tool input:

```json
{
  "to_representative_agent_id": "rep_target",
  "text": "nihao",
  "include_message": false
}
```

Alternative file input:

```json
{
  "to_representative_agent_id": "rep_target",
  "input_file": "./question.md",
  "include_message": true
}
```

paxd sends manager request:

```json
{
  "source": {
    "agent_id": "$PAX_AGENT_ID",
    "representative_agent_id": "$PAX_REPRESENTATIVE_AGENT_ID",
    "session_id": "$PAX_SESSION_ID"
  },
  "target": {
    "kind": "representative",
    "representative_agent_id": "rep_target"
  },
  "context": {
    "latest_response": false
  },
  "instruction": "nihao"
}
```

Successful MCP result:

```json
{
  "ok": true,
  "status": "sent",
  "receipt_token": "rcpt_abc",
  "message": "Invocation sent."
}
```

Human-readable CLI-style summary:

```text
sent receipt=rcpt_abc
```

Failed MCP result:

```json
{
  "ok": false,
  "error": "target session already has an active invocation"
}
```

## Reply Tool

`reply` sends a response for the current session's only active invocation. It does
not take a reply token, invocation id, target agent id, target session id, or
conversation id.

Tool input:

```json
{
  "text": "wozhidaodaan",
  "include_message": false
}
```

Alternative file input:

```json
{
  "input_file": "./answer.md",
  "include_message": true
}
```

paxd sends manager request:

```json
{
  "source": {
    "agent_id": "$PAX_AGENT_ID",
    "representative_agent_id": "$PAX_REPRESENTATIVE_AGENT_ID",
    "session_id": "$PAX_SESSION_ID"
  },
  "target": {
    "kind": "active_invocation"
  },
  "context": {
    "latest_response": false
  },
  "instruction": "wozhidaodaan"
}
```

Manager behavior:

1. Find the active invocation for `source.agent_id + source.session_id`.
2. If none exists, return `no active invocation for session`.
3. If more than one exists, return `ambiguous active invocation`.
4. Verify the current source is the original invocation target.
5. Write the reply as a conversation message from the current source session.
6. Mark the active invocation completed and release the target session lock.

Successful MCP result:

```json
{
  "ok": true,
  "status": "sent",
  "message": "Reply sent."
}
```

Human-readable CLI-style summary:

```text
sent
```

## Manager State

For v1, do not add a separate inquiries table. Treat each
`conversation_agent_invocations` row as one active unit of work for the target
session. The existing invocation `status` carries the lifecycle state.

```text
conversation_agent_invocations
- receipt_token_hash
- status: active/completed/cancelled/expired
- expires_at
```

Required invariant:

```sql
CREATE UNIQUE INDEX idx_active_invocation_target_session
  ON conversation_agent_invocations (target_runtime_agent_id, target_session_id)
  WHERE status = 'active';
```

The receipt token is returned only to the ask caller. Store only a hash in the
database. The token can later support a `status` tool, audit lookup, cancellation,
or timeout diagnostics.

Ask write path:

1. Resolve the target representative to one target runtime/session.
2. Create the ask invocation with `status = 'active'`.
3. Store `receipt_token_hash`, not the raw receipt token.
4. Return the raw receipt token to the caller.

Reply write path:

1. Find the active invocation where the current session is the target.
2. Write the reply message in the conversation.
3. Mark the invocation `status = 'completed'`.

Multiturn should create a new invocation explicitly. A future `reply_and_ask`
operation can atomically complete the current invocation and create the next
one, but plain `reply` does not create a child invocation.

Create a dedicated task or target table later only if the model needs multiple
parallel targets, group fanout, or richer task lifecycle that becomes awkward on
the invocation table.

## Compatibility Notes

The existing one-shot CLI is still useful for local smoke tests:

```sh
paxd mcp conversation ask --to-representative-agent-id rep_123 "nihao"
paxd mcp conversation reply "wozhidaodaan"
```

However, the target ACP integration should use `paxd mcp conversation serve` as a
stdio MCP server registered once at `session/new`.
