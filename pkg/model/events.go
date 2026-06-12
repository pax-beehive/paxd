package model

// ═══════════════════════════════════════════════════════════════════
// Backend → Client Events
// ═══════════════════════════════════════════════════════════════════

// ── Entity: session ──

// SessionCreated is the first event after a client connects or creates a session.
// It declares the agent's capabilities so the frontend can adapt its UI.
type SessionCreated struct {
	SessionBase                         // entity_type="session", event_type="created"
	AgentType    string                 `json:"agentType"`
	Capabilities *CapabilityDecl        `json:"capabilities"`
	CWD          string                 `json:"cwd"`
	Model        string                 `json:"model"`
	Roots        []string               `json:"roots,omitempty"`
}

// SessionResumed is sent when a client reconnects to an existing session.
type SessionResumed struct {
	SessionBase                      // entity_type="session", event_type="resumed"
	LastResponseID string            `json:"lastResponseId"`
}

// ── Entity: turn ──

// TurnStarted marks the beginning of a new turn.
// The frontend resets its message buffer and spinner state.
type TurnStarted struct {
	TurnBase // entity_type="turn", event_type="started"
}

// TurnDone marks the end of a turn.
type TurnDone struct {
	TurnBase                    // entity_type="turn", event_type="done"
	ResponseID string           `json:"responseId,omitempty"`
	Status     string           `json:"status"` // "completed" | "partial" | "cancelled"
	Usage      *UsageInfo       `json:"usage,omitempty"`
}

// TurnError is sent when a turn fails before producing a meaningful response.
type TurnError struct {
	TurnBase             // entity_type="turn", event_type="error"
	Error     string     `json:"error"`
	Code      string     `json:"code"` // "backend_unavailable" | "timeout" | "rate_limited" | …
}

// ── Entity: message ──

// MessageDelta is an incremental text chunk emitted during streaming.
// Role="assistant" → main chat bubble. Role="tool" → dimmed tool preview (┊ prefix).
//
// There is no "message.done" event — AgentStatus with status="done" also signals completion.
type MessageDelta struct {
	TurnBase                // entity_type="message", event_type="delta"
	Role     string         `json:"role"`    // "assistant" | "tool"
	Content  string         `json:"content"` // incremental text
}

// ── Entity: tool ──

// ToolCall is emitted when the agent decides to invoke a tool.
type ToolCall struct {
	TurnBase                // entity_type="tool", event_type="call"
	CallID   string         `json:"callId"`
	Name     string         `json:"name"`
	Arguments string        `json:"arguments"` // JSON string of args
}

// ToolResult is the outcome of a tool execution.
type ToolResult struct {
	TurnBase                // entity_type="tool", event_type="result"
	CallID    string        `json:"callId"`
	Output    string        `json:"output"`
	Error     string        `json:"error,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
}

// ToolApprovalRequired is emitted when the agent wants to run a dangerous command.
// The backend blocks until the client responds with ToolApprove or ToolDeny.
type ToolApprovalRequired struct {
	TurnBase                // entity_type="tool", event_type="approval_required"
	CallID    string        `json:"callId"`
	Name      string        `json:"name"`
	Arguments string        `json:"arguments"`
	Reason    string        `json:"reason"` // human-readable risk explanation
}

// ── Entity: agent ──

// AgentStatus is a display instruction for the frontend.
// The frontend switches on Status and renders Label, Icon, Detail directly —
// zero inference, no state machine.
//
// LIVE-ONLY: never persisted or included in history replay.
type AgentStatus struct {
	SessionBase                     // entity_type="agent", event_type="status"
	TurnID      string             `json:"turnId,omitempty"` // empty when idle
	Status      string             `json:"status"`  // "idle" | "thinking" | "working" | "done" | "error" | "cancelled"
	Label       string             `json:"label"`   // e.g. "Thinking…"
	Icon        string             `json:"icon"`    // e.g. "🧠"
	Detail      string             `json:"detail,omitempty"`  // e.g. "📖 read_file handler.go"
	Progress    float64            `json:"progress,omitempty"` // 0.0–1.0
}

// ── Entity: file ──

// FileChanged is emitted at turn end (before TurnDone) with all files modified
// by write_file / patch tools during this turn.
type FileChanged struct {
	TurnBase                 // entity_type="file", event_type="changed"
	Changes  []*FileChange   `json:"changes"`
}
