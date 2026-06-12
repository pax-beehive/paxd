package model

// ═══════════════════════════════════════════════════════════════════
// Client → Backend Requests
// ═══════════════════════════════════════════════════════════════════

// ── Entity: session ──

// SessionCreate is a request to start a new session.
type SessionCreate struct {
	Envelope                  // entity_type="session", event_type="create"
	AgentType string          `json:"agentType"`
	CWD       string          `json:"cwd"`
	Model     string          `json:"model,omitempty"`
	Roots     []string        `json:"roots,omitempty"`
	Name      string          `json:"name,omitempty"`
}

// SessionResume is a request to reconnect to an existing session.
type SessionResume struct {
	Envelope               // entity_type="session", event_type="resume"
	SessionID string       `json:"sessionId"`
}

// SessionList is a request to enumerate all sessions across all agents.
type SessionList struct {
	Envelope // entity_type="session", event_type="list"
}

// SessionListResult is the response to SessionList.
type SessionListResult struct {
	Envelope                // entity_type="session", event_type="list_result"
	Sessions []*SessionInfo `json:"sessions"`
}

// SessionDelete is a request to delete a session and its native data.
type SessionDelete struct {
	Envelope               // entity_type="session", event_type="delete"
	SessionID string       `json:"sessionId"`
}

// SessionHistory is a request to load the full history of a session.
// The backend responds with a stream of events (MessageDelta, ToolCall, etc.).
type SessionHistory struct {
	Envelope               // entity_type="session", event_type="history"
	SessionID string       `json:"sessionId"`
}

// ── Entity: turn ──

// TurnStart is a request to begin a new turn with the given prompt.
type TurnStart struct {
	Envelope               // entity_type="turn", event_type="start"
	SessionID  string      `json:"sessionId"`
	Prompt     string      `json:"prompt"`
	SideBranch bool        `json:"sideBranch,omitempty"`
}

// TurnCancel is a request to cancel/stop the current turn.
type TurnCancel struct {
	Envelope              // entity_type="turn", event_type="cancel"
	SessionID string      `json:"sessionId"`
	Mode      string      `json:"mode"` // "stop" | "steer"
	SteerText string      `json:"steerText,omitempty"`
}

// ── Entity: tool ──

// ToolApprove grants permission for a pending tool call.
type ToolApprove struct {
	Envelope              // entity_type="tool", event_type="approve"
	SessionID string      `json:"sessionId"`
	TurnID    string      `json:"turnId"`
	CallID    string      `json:"callId"`
	Scope     string      `json:"scope,omitempty"` // "once" | "session" | "always"
}

// ToolDeny rejects a pending tool call.
type ToolDeny struct {
	Envelope              // entity_type="tool", event_type="deny"
	SessionID string      `json:"sessionId"`
	TurnID    string      `json:"turnId"`
	CallID    string      `json:"callId"`
}
