# Cancelling a turn waiting for permission

After forwarding `session/cancel`, the ACP router responds to outstanding
permission requests from that session and slot epoch with the ACP cancelled
outcome: `{"outcome":{"outcome":"cancelled"}}`. This releases agents that
are waiting for a client permission response before they can finish cancelling.
It never selects an allow option. Requests from other sessions are preserved.

Cancellation does not release the active prompt lease immediately. The router
still waits for the agent's terminal prompt response before admitting the next
turn. The same router path is used for ordinary and encrypted sessions.
