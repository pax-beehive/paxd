package model

// ═══════════════════════════════════════════════════════════════════
// Envelope Hierarchy
// ═══════════════════════════════════════════════════════════════════

// Envelope is the two-field routing header for every message.
// Small enough (2 strings) to pass by value.
type Envelope struct {
	EntityType string `json:"entity_type"` // "session" | "turn" | "message" | "tool" | "agent" | "file" | …
	EventType  string `json:"event_type"`  // "created" | "delta" | "call" | "status" | …
}

// SessionBase adds a SessionID to the envelope. Used by all session-scoped events.
// Pass by pointer.
type SessionBase struct {
	Envelope
	SessionID string `json:"sessionId"`
}

// TurnBase adds a TurnID on top of SessionBase. Used by all turn-scoped events.
// Pass by pointer.
type TurnBase struct {
	SessionBase
	TurnID string `json:"turnId"`
}
