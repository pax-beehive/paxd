package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type attributionHarness struct {
	router  *ACPRouter
	emitted []string
}

func newAttributionHarness(t *testing.T) *attributionHarness {
	t.Helper()
	harness := &attributionHarness{}
	store := newFakeACPRouteStore("conn_1")
	harness.router = newTestACPRouter("conn_1", store, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(
		func(ctx context.Context, _ string, _ []byte) error {
			harness.emitted = append(harness.emitted, acpTurnID(ctx))
			return nil
		},
	)))
	harness.router.UpsertSlot(newFakeRouterSlot("slot_a", "epoch_a", 0))
	store.seedRoute(ACPRoute{ConnectionID: "conn_1", NativeSessionID: "session_1",
		BoundSlotID: "slot_a", BoundProcessEpoch: "epoch_a", Version: 1,
		ResumeParams: json.RawMessage(`{"cwd":"/work","mcpServers":[]}`)})
	return harness
}

func (h *attributionHarness) startPrompt(t *testing.T, id int, turnID string) {
	t.Helper()
	prompt := `{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"session/prompt","params":{"sessionId":"session_1","prompt":[]}}`
	require.NoError(t, h.router.HandleManagerFrame(withACPTurnID(t.Context(), turnID), []byte(prompt)))
}

func (h *attributionHarness) answerPrompt(t *testing.T, id int) {
	t.Helper()
	response := `{"jsonrpc":"2.0","id":` + itoa(id) + `,"result":{"stopReason":"end_turn"}}`
	require.NoError(t, h.router.HandleSlotFrame(t.Context(), "slot_a", "epoch_a", []byte(response)))
}

// update emits one worker session/update and returns the turn it went out on.
func (h *attributionHarness) update(t *testing.T, body string) string {
	t.Helper()
	frame := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session_1","update":` + body + `}}`
	require.NoError(t, h.router.HandleSlotFrame(t.Context(), "slot_a", "epoch_a", []byte(frame)))
	return h.emitted[len(h.emitted)-1]
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestSubagentOutputStaysOnLaunchingTurnAfterPromptAnswers(t *testing.T) {
	h := newAttributionHarness(t)
	h.startPrompt(t, 10, "turn_first")
	h.update(t, `{"sessionUpdate":"tool_call","toolCallId":"agent_1","_meta":{"claudeCode":{"toolName":"Agent"}}}`)
	h.answerPrompt(t, 10)

	// A background subagent keeps streaming after the prompt answered. The tool
	// hook refinement of its call carries no parent, only the tool call id.
	assert.Equal(t, "turn_first", h.update(t,
		`{"sessionUpdate":"tool_call","toolCallId":"child_1","_meta":{"claudeCode":{"toolName":"Read","parentToolUseId":"agent_1"}}}`))
	assert.Equal(t, "turn_first", h.update(t,
		`{"sessionUpdate":"tool_call_update","toolCallId":"child_1","_meta":{"claudeCode":{"toolName":"Read"}}}`))
	assert.Equal(t, "turn_first", h.update(t,
		`{"sessionUpdate":"tool_call_update","toolCallId":"agent_1","status":"completed"}`))
	assert.Empty(t, h.router.RuntimeSnapshot().ActiveTurns, "subagent output must not open a background turn")
}

func TestToolCallUpdateFollowsItsCallIntoTheNextTurn(t *testing.T) {
	h := newAttributionHarness(t)
	h.startPrompt(t, 10, "turn_first")
	h.update(t, `{"sessionUpdate":"tool_call","toolCallId":"bash_1"}`)
	h.answerPrompt(t, 10)
	h.startPrompt(t, 11, "turn_second")

	assert.Equal(t, "turn_first", h.update(t, `{"sessionUpdate":"tool_call_update","toolCallId":"bash_1","status":"completed"}`))
	assert.Equal(t, "turn_second", h.update(t, `{"sessionUpdate":"tool_call","toolCallId":"bash_1"}`),
		"a reused tool call id announced again belongs to the current turn")
}

func TestAutonomousReplyRunsAsBackgroundTurn(t *testing.T) {
	h := newAttributionHarness(t)
	h.startPrompt(t, 10, "turn_first")
	h.answerPrompt(t, 10)

	assert.Equal(t, "turn_first", h.update(t, `{"sessionUpdate":"usage_update","used":10,"size":100}`))
	require.Empty(t, h.router.RuntimeSnapshot().ActiveTurns, "a usage update alone does not start work")

	backgroundTurnID := h.update(t, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"The subagent found"}}`)
	require.NotEmpty(t, backgroundTurnID)
	require.NotEqual(t, "turn_first", backgroundTurnID)
	turns := h.router.RuntimeSnapshot().ActiveTurns
	require.Len(t, turns, 1)
	assert.Equal(t, backgroundTurnID, turns[0].TurnID)
	assert.Equal(t, SessionRuntimeRunning, turns[0].RuntimeStatus)
	var requestID string
	require.NoError(t, json.Unmarshal(turns[0].PromptRequestID, &requestID))
	assert.True(t, strings.HasPrefix(requestID, backgroundPromptRequestPrefix))

	assert.Equal(t, backgroundTurnID, h.update(t, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":" five files."}}`))
	assert.Equal(t, backgroundTurnID, h.update(t,
		`{"sessionUpdate":"usage_update","used":12,"size":100,"_meta":{"_claude/origin":{"kind":"task-notification"}}}`),
		"the frame that ends the cycle still belongs to it")
	assert.Empty(t, h.router.RuntimeSnapshot().ActiveTurns)

	next := h.update(t, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Another task finished."}}`)
	assert.NotEqual(t, backgroundTurnID, next, "each autonomous cycle is its own turn")
}

func TestPromptTakesOverBackgroundTurn(t *testing.T) {
	h := newAttributionHarness(t)
	h.startPrompt(t, 10, "turn_first")
	h.answerPrompt(t, 10)
	h.update(t, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Woke up"}}`)
	require.Len(t, h.router.RuntimeSnapshot().ActiveTurns, 1)

	h.startPrompt(t, 11, "turn_second")
	turns := h.router.RuntimeSnapshot().ActiveTurns
	require.Len(t, turns, 1)
	assert.Equal(t, "turn_second", turns[0].TurnID)
	assert.Equal(t, "turn_second", h.update(t, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Hi"}}`))
}

func TestOutputBeforeAnyPromptOpensNoBackgroundTurn(t *testing.T) {
	h := newAttributionHarness(t)
	assert.Empty(t, h.update(t, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"replayed"}}`))
	assert.Empty(t, h.router.RuntimeSnapshot().ActiveTurns)
}

func TestRemovingSlotEndsBackgroundTurn(t *testing.T) {
	h := newAttributionHarness(t)
	h.startPrompt(t, 10, "turn_first")
	h.answerPrompt(t, 10)
	h.update(t, `{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Woke up"}}`)
	require.Len(t, h.router.RuntimeSnapshot().ActiveTurns, 1)

	h.router.RemoveSlot("slot_a", "epoch_a")
	assert.Empty(t, h.router.RuntimeSnapshot().ActiveTurns)
}
