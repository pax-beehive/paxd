package runtime

import (
	"bytes"
	"encoding/json"
)

// Scope to the worker process as well as the session: a slot may host multiple
// sessions and its replacement must not inherit an old process's replay guard.
type resumeTranscriptScope struct {
	nativeSessionID string
	slotID          string
	processEpoch    string
}

// Returns whether this scope was previously suppressed. A failed send can
// restore the guard; failed/cancelled resumes retain it until retry or teardown.
func (r *ACPRouter) setResumeTranscriptSuppressed(scope resumeTranscriptScope, suppress bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, previous := r.resumeTranscripts[scope]
	if suppress {
		// A caller resuming an already running session must not silence its live turn.
		if r.activeSessionTurns[scope.nativeSessionID] != "" {
			return previous
		}
		if r.resumeTranscripts == nil {
			r.resumeTranscripts = make(map[resumeTranscriptScope]struct{})
		}
		r.resumeTranscripts[scope] = struct{}{}
	} else {
		delete(r.resumeTranscripts, scope)
	}
	return previous
}

func (r *ACPRouter) suppressResumeTranscript(sessionID, slotID, epoch string, msg acpRPCMessage) bool {
	// Never swallow RPC requests (permissions, filesystem operations, etc.) or
	// responses. Only known transcript notifications are safe to discard.
	if msg.Method != "session/update" || len(bytes.TrimSpace(msg.ID)) != 0 {
		return false
	}
	var params struct {
		Update struct {
			Kind string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if json.Unmarshal(msg.Params, &params) != nil {
		return false
	}
	switch params.Update.Kind {
	case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update", "plan":
	default:
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, suppressed := r.resumeTranscripts[resumeTranscriptScope{sessionID, slotID, epoch}]
	return suppressed
}
