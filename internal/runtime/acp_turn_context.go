package runtime

import (
	"context"
	"encoding/json"
)

type acpTurnContextKey struct{}

// The turn belongs to the transport envelope, never the worker's ACP payload.
func withACPTurnID(ctx context.Context, turnID string) context.Context {
	return context.WithValue(ctx, acpTurnContextKey{}, turnID)
}

func acpTurnID(ctx context.Context) string {
	turnID, _ := ctx.Value(acpTurnContextKey{}).(string)
	return turnID
}

func (r *ACPRouter) outputTurnContext(ctx context.Context, sessionID, slotID, epoch string) context.Context {
	turn, ok := r.projector.ActiveTurn(sessionID)
	if !ok || turn.SlotID != slotID || turn.ProcessEpoch != epoch {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i := len(r.completedPromptKeys) - 1; i >= 0; i-- {
			old := r.completedPrompts[r.completedPromptKeys[i]]
			if old.nativeSessionID == sessionID && old.slotID == slotID && old.processEpoch == epoch {
				return withACPTurnID(ctx, old.turnID)
			}
		}
		return withACPTurnID(ctx, "")
	}
	return withACPTurnID(ctx, turn.TurnID)
}

// Capture the request's identity before releasing its lease, including late responses.
func (r *ACPRouter) promptTurnForResponse(id json.RawMessage, slotID, epoch string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	pending, ok := r.pendingPrompts[rpcIDKey(id)]
	if !ok {
		pending, ok = r.completedPrompts[internalWaiterKey(slotID, epoch, id)]
	}
	if !ok || pending.slotID != slotID || pending.processEpoch != epoch {
		return ""
	}
	return pending.turnID
}

func (r *ACPRouter) rememberPromptLocked(requestKey string, prompt pendingPrompt) {
	if prompt.turnID == "" {
		return
	}
	if r.completedPrompts == nil {
		r.completedPrompts = make(map[string]pendingPrompt)
	}
	key := prompt.slotID + "|" + prompt.processEpoch + "|" + requestKey
	if _, exists := r.completedPrompts[key]; exists {
		return
	}
	r.completedPrompts[key] = prompt
	r.completedPromptKeys = append(r.completedPromptKeys, key)
	if len(r.completedPromptKeys) > sessionRuntimeTerminalLimit {
		delete(r.completedPrompts, r.completedPromptKeys[0])
		r.completedPromptKeys = r.completedPromptKeys[1:]
	}
}
