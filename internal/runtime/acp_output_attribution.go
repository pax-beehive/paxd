package runtime

import (
	"context"
	"encoding/json"
	"strings"
)

// Claude Code keeps producing output after a prompt answers: background
// subagents stream tool calls, tool hooks refine earlier calls, and a finished
// background task wakes the main agent for a reply that no prompt asked for.
// These rules keep that output on the turn it belongs to, and project the
// autonomous reply as a background turn so the session reads as running while
// it streams.

const (
	backgroundPromptRequestPrefix = "background:"
	toolCallTurnLimit             = 4096
)

type backgroundTurn struct {
	turnID       string
	slotID       string
	processEpoch string
}

type subagentTurn struct {
	turnID    string
	completed bool
}

type sessionUpdateFacts struct {
	kind             string
	toolCallID       string
	parentToolUseID  string
	autonomousEnd    bool
	subagentThreadID string
	subagentActivity string
}

func parseSessionUpdateFacts(params json.RawMessage) sessionUpdateFacts {
	var envelope struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			ToolCallID    string          `json:"toolCallId"`
			RawInput      json.RawMessage `json:"rawInput"`
			Meta          struct {
				ClaudeCode struct {
					ParentToolUseID string `json:"parentToolUseId"`
				} `json:"claudeCode"`
				Origin *struct {
					Kind string `json:"kind"`
				} `json:"_claude/origin"`
			} `json:"_meta"`
		} `json:"update"`
	}
	if json.Unmarshal(params, &envelope) != nil {
		return sessionUpdateFacts{}
	}
	update := envelope.Update
	origin := update.Meta.Origin
	var activity struct {
		Kind     string `json:"activityKind"`
		ThreadID string `json:"agentThreadId"`
	}
	// Other tools may use a string or array as input. Invalid activity data
	// must not discard their tool call identity or Claude attribution.
	_ = json.Unmarshal(update.RawInput, &activity)
	return sessionUpdateFacts{
		kind:             update.SessionUpdate,
		toolCallID:       strings.TrimSpace(update.ToolCallID),
		parentToolUseID:  strings.TrimSpace(update.Meta.ClaudeCode.ParentToolUseID),
		subagentThreadID: strings.TrimSpace(activity.ThreadID),
		subagentActivity: strings.TrimSpace(activity.Kind),
		// claude-agent-acp stamps the origin of a cycle it did not start for a
		// prompt on the usage update it sends at that cycle's result.
		autonomousEnd: update.SessionUpdate == "usage_update" &&
			origin != nil && origin.Kind != "" && origin.Kind != "human",
	}
}

// opensBackgroundTurn reports whether the update is main-agent work, which
// only a prompt or an autonomous cycle produces.
func (f sessionUpdateFacts) opensBackgroundTurn() bool {
	if f.parentToolUseID != "" {
		return false
	}
	switch f.kind {
	case "agent_message_chunk", "agent_thought_chunk", "tool_call", "plan":
		return true
	default:
		return false
	}
}

// sessionUpdateTurnContext attributes a worker session/update. The returned
// func runs after the frame is emitted and closes the background turn that the
// frame ended, so the closing frame still carries that turn.
func (r *ACPRouter) sessionUpdateTurnContext(
	ctx context.Context,
	nativeSessionID string,
	slotID string,
	processEpoch string,
	params json.RawMessage,
) (context.Context, func()) {
	noop := func() {}
	if nativeSessionID == "" {
		return r.outputTurnContext(ctx, nativeSessionID, slotID, processEpoch), noop
	}
	facts := parseSessionUpdateFacts(params)
	if facts.kind == "tool_call" && facts.subagentThreadID != "" && facts.subagentActivity != "" {
		return r.subagentActivityTurnContext(ctx, nativeSessionID, slotID, processEpoch, facts), noop
	}
	if turnID, ok := r.attributedToolCallTurn(nativeSessionID, facts); ok {
		return withACPTurnID(ctx, turnID), noop
	}
	if facts.opensBackgroundTurn() {
		r.ensureBackgroundTurn(nativeSessionID, slotID, processEpoch)
	}
	ctx = r.outputTurnContext(ctx, nativeSessionID, slotID, processEpoch)
	if facts.kind == "tool_call" {
		r.rememberToolCallTurn(nativeSessionID, facts.toolCallID, acpTurnID(ctx))
	}
	if facts.autonomousEnd {
		return ctx, func() { r.completeBackgroundTurn(nativeSessionID, slotID, processEpoch) }
	}
	return ctx, noop
}

// Codex announces lifecycle events with different tool call ids for the same
// child thread. These notifications belong to its work, not a new main turn.
func (r *ACPRouter) subagentActivityTurnContext(
	ctx context.Context,
	nativeSessionID string,
	slotID string,
	processEpoch string,
	facts sessionUpdateFacts,
) context.Context {
	ctx = r.outputTurnContext(ctx, nativeSessionID, slotID, processEpoch)
	r.mu.Lock()
	defer r.mu.Unlock()

	// Lifecycle event ids are stable across replays, including a replay of a
	// start or interaction after the child has already moved to a new turn.
	if turnID := r.toolCallTurns[toolCallTurnKey(nativeSessionID, facts.toolCallID)]; turnID != "" {
		return withACPTurnID(ctx, turnID)
	}
	key := toolCallTurnKey(nativeSessionID, facts.subagentThreadID)
	owner, exists := r.subagentTurns[key]
	if !exists || facts.subagentActivity == "started" || (facts.subagentActivity == "interacted" && owner.completed) {
		owner = subagentTurn{turnID: acpTurnID(ctx)}
	}
	owner.completed = facts.subagentActivity == "completed"
	if owner.turnID != "" {
		if _, exists := r.subagentTurns[key]; !exists {
			r.subagentTurnKeys = append(r.subagentTurnKeys, key)
		}
		r.subagentTurns[key] = owner
		if len(r.subagentTurnKeys) > toolCallTurnLimit {
			delete(r.subagentTurns, r.subagentTurnKeys[0])
			r.subagentTurnKeys = r.subagentTurnKeys[1:]
		}
	}
	r.rememberToolCallTurnLocked(nativeSessionID, facts.toolCallID, owner.turnID)
	return withACPTurnID(ctx, owner.turnID)
}

// attributedToolCallTurn keeps a tool call update on the turn that announced
// the tool call, and subagent output on the turn that launched the subagent.
// An announcement always takes the current turn, so a harness that reuses
// tool call ids cannot pull a new call into an old turn.
func (r *ACPRouter) attributedToolCallTurn(nativeSessionID string, facts sessionUpdateFacts) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if facts.toolCallID != "" && facts.kind != "tool_call" {
		if turnID := r.toolCallTurns[toolCallTurnKey(nativeSessionID, facts.toolCallID)]; turnID != "" {
			return turnID, true
		}
	}
	if facts.parentToolUseID == "" {
		return "", false
	}
	turnID := r.toolCallTurns[toolCallTurnKey(nativeSessionID, facts.parentToolUseID)]
	if turnID == "" {
		return "", false
	}
	if facts.toolCallID != "" {
		r.rememberToolCallTurnLocked(nativeSessionID, facts.toolCallID, turnID)
	}
	return turnID, true
}

func (r *ACPRouter) rememberToolCallTurn(nativeSessionID string, toolCallID string, turnID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rememberToolCallTurnLocked(nativeSessionID, toolCallID, turnID)
}

func (r *ACPRouter) rememberToolCallTurnLocked(nativeSessionID string, toolCallID string, turnID string) {
	if toolCallID == "" || turnID == "" {
		return
	}
	key := toolCallTurnKey(nativeSessionID, toolCallID)
	if _, exists := r.toolCallTurns[key]; !exists {
		r.toolCallTurnKeys = append(r.toolCallTurnKeys, key)
	}
	r.toolCallTurns[key] = turnID
	if len(r.toolCallTurnKeys) > toolCallTurnLimit {
		delete(r.toolCallTurns, r.toolCallTurnKeys[0])
		r.toolCallTurnKeys = r.toolCallTurnKeys[1:]
	}
}

// ensureBackgroundTurn starts a turn for main-agent output that arrives while
// no prompt runs. Claude Code only starts such a cycle after a prompt ran in
// the same process, which also keeps a history replay from opening one.
func (r *ACPRouter) ensureBackgroundTurn(nativeSessionID string, slotID string, processEpoch string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeSessionTurns[nativeSessionID] != "" {
		return
	}
	if _, exists := r.backgroundTurns[nativeSessionID]; exists {
		return
	}
	if _, active := r.projector.ActiveTurn(nativeSessionID); active {
		return
	}
	if !r.sessionPromptedOnProcessLocked(nativeSessionID, slotID, processEpoch) {
		return
	}
	turnID, err := newSessionRuntimeTurnID()
	if err != nil {
		return
	}
	requestID, err := json.Marshal(backgroundPromptRequestPrefix + turnID)
	if err != nil {
		return
	}
	if _, err := r.projector.StartTurn(nativeSessionID, requestID, slotID, processEpoch, turnID); err != nil {
		return
	}
	r.backgroundTurns[nativeSessionID] = backgroundTurn{
		turnID:       turnID,
		slotID:       slotID,
		processEpoch: processEpoch,
	}
}

func (r *ACPRouter) sessionPromptedOnProcessLocked(nativeSessionID string, slotID string, processEpoch string) bool {
	for _, key := range r.completedPromptKeys {
		prompt := r.completedPrompts[key]
		if prompt.nativeSessionID == nativeSessionID && prompt.slotID == slotID && prompt.processEpoch == processEpoch {
			return true
		}
	}
	return false
}

func (r *ACPRouter) completeBackgroundTurn(nativeSessionID string, slotID string, processEpoch string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	turn, ok := r.backgroundTurns[nativeSessionID]
	if !ok || turn.slotID != slotID || turn.processEpoch != processEpoch {
		return
	}
	r.completeBackgroundTurnLocked(nativeSessionID)
}

func (r *ACPRouter) completeBackgroundTurnLocked(nativeSessionID string) {
	turn, ok := r.backgroundTurns[nativeSessionID]
	if !ok {
		return
	}
	delete(r.backgroundTurns, nativeSessionID)
	r.projector.CompleteTurn(nativeSessionID, turn.turnID)
}

func (r *ACPRouter) completeBackgroundTurnsForSlotLocked(slotID string, processEpoch string) {
	for nativeSessionID, turn := range r.backgroundTurns {
		if turn.slotID == slotID && (processEpoch == "" || turn.processEpoch == processEpoch) {
			r.completeBackgroundTurnLocked(nativeSessionID)
		}
	}
}

func toolCallTurnKey(nativeSessionID string, toolCallID string) string {
	return nativeSessionID + "\x00" + toolCallID
}
