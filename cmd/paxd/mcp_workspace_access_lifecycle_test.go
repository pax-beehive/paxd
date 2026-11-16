package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceAccessGivenOptionalTimeoutThenPreservesNoDeadlineAndExplicitTimeouts(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	t.Setenv(envPaxAgentID, "agent-caller")
	t.Setenv(envPaxSessionID, "caller-session")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	for _, tc := range []struct{ name, args, expected string }{
		{"omitted", `{}`, `{"queries":["*"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["*"]}`},
		{"no deadline", `{"timeout_seconds":0}`, `{"queries":["*"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["*"],"timeout_seconds":0}`},
		{"default explicit", `{"timeout_seconds":600}`, `{"queries":["*"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["*"],"timeout_seconds":600}`},
		{"maximum", `{"timeout_seconds":86400}`, `{"queries":["*"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["*"],"timeout_seconds":86400}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			callOrdinaryWorkspace = func(_ context.Context, agentID string, request cloud.WorkspaceToolRequest) (json.RawMessage, error) {
				calls++
				assert.Equal(t, "agent-caller", agentID)
				assert.Equal(t, "caller-session", request.SessionID)
				assert.Equal(t, "request_access", request.Tool)
				assert.JSONEq(t, tc.expected, request.ArgumentsJSON)
				return json.RawMessage(`{"status":"pending","approval_id":"approval-1","turn_id":"turn-1","scope":{},"poll_after_ms":2000}`), nil
			}
			result, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_request_access","arguments":`+tc.args+`}`))
			require.NoError(t, err)
			assert.False(t, result.IsError)
			assert.Equal(t, 1, calls, "a pending request must not be polled automatically")
		})
	}
	for _, tool := range ordinaryWorkspaceMCPTools() {
		if tool["name"] != "workspace_request_access" {
			continue
		}
		schema := tool["inputSchema"].(workspaceSchema)
		timeout := schema["properties"].(map[string]any)["timeout_seconds"].(workspaceSchema)
		assert.Equal(t, 600, timeout["default"])
		assert.EqualValues(t, 0, timeout["minimum"])
		assert.EqualValues(t, 86400, timeout["maximum"])
	}
}

func TestWorkspaceAccessGivenApprovalIDThenQueriesAndCancelsWithoutWaitingOrRewritingState(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	t.Setenv(envPaxAgentID, "agent-caller")
	t.Setenv(envPaxSessionID, "caller-session")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	for _, tc := range []struct{ name, wire string }{
		{"workspace_access_status", "access_status"},
		{"workspace_cancel_access", "cancel_access"},
	} {
		for _, status := range []string{"pending", "approved", "denied", "expired", "cancelled"} {
			t.Run(tc.name+"/"+status, func(t *testing.T) {
				response := `{"status":"` + status + `","approval_id":"approval-1","turn_id":"turn-1","scope":{"queries":["cicd"],"node_ids":["node-target"],"agent_ids":["agent-target"],"session_ids":["*"]},"expires_at":"2026-11-11T23:00:00Z","poll_after_ms":2000}`
				calls := 0
				callOrdinaryWorkspace = func(_ context.Context, agentID string, request cloud.WorkspaceToolRequest) (json.RawMessage, error) {
					calls++
					assert.Equal(t, "agent-caller", agentID)
					assert.Equal(t, "caller-session", request.SessionID)
					assert.Equal(t, tc.wire, request.Tool)
					assert.JSONEq(t, `{"approval_id":"approval-1"}`, request.ArgumentsJSON)
					return json.RawMessage(response), nil
				}
				result, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"`+tc.name+`","arguments":{"approval_id":"approval-1"}}`))
				require.NoError(t, err)
				assert.False(t, result.IsError)
				assert.JSONEq(t, response, result.Content[0].Text)
				raw, err := json.Marshal(result.StructuredContent)
				require.NoError(t, err)
				assert.JSONEq(t, response, string(raw))
				assert.Equal(t, 1, calls, "status and cancellation must not wait, retry, or create another request")
			})
		}
	}
}

func TestWorkspaceAccessGivenInvalidTimeoutOrForgedStatusIdentityThenDoesNotSend(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	callOrdinaryWorkspace = func(context.Context, string, cloud.WorkspaceToolRequest) (json.RawMessage, error) {
		t.Fatal("invalid access lifecycle arguments reached Manager")
		return nil, nil
	}
	for _, value := range []string{"-1", "86401", "0.5", `"600"`, "true", "null"} {
		_, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_request_access","arguments":{"timeout_seconds":`+value+`}}`))
		require.Error(t, err)
	}
	for _, tool := range []string{"workspace_access_status", "workspace_cancel_access"} {
		for _, args := range []string{
			`{}`, `{"approval_id":""}`, `{"approval_id":" "}`, `{"approval_id":123}`,
			`{"approval_id":"approval-1","owner_user_id":"foreign"}`,
			`{"approval_id":"approval-1","agent_id":"foreign"}`,
			`{"approval_id":"approval-1","session_id":"foreign"}`,
			`{"approval_id":"approval-1","turn_id":"old-turn"}`,
			`{"approval_id":"approval-1","capability":"forged"}`,
		} {
			_, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"`+tool+`","arguments":`+args+`}`))
			require.Error(t, err)
		}
	}
}
