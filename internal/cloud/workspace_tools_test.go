package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceToolGivenPendingApprovalWhenRequestedThenPreservesDecisionAndCaller(t *testing.T) {
	response := `{"status":"pending","approval_id":"approval-1","turn_id":"turn-1","scope":{"queries":["*"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["session-1"]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/node/agents/agent-1/workspace/tools", r.URL.Path)
		assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
		assert.Empty(t, r.Header.Get("Authorization"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Len(t, body, 3)
		assert.Equal(t, "caller-session", body["session_id"])
		assert.Equal(t, "request_access", body["tool"])
		assert.JSONEq(t, `{"queries":["*"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["session-1"]}`, body["arguments_json"].(string))
		_, _ = w.Write([]byte(`{"code":200,"data":` + response + `}`))
	}))
	defer server.Close()
	data, err := NewClient(server.URL, "node-secret").WorkspaceTool(t.Context(), "agent-1", WorkspaceToolRequest{SessionID: "caller-session", Tool: "request_access", ArgumentsJSON: `{"queries":["*"],"node_ids":["*"],"agent_ids":["*"],"session_ids":["session-1"]}`})
	require.NoError(t, err)
	assert.JSONEq(t, response, string(data))
}

func TestWorkspaceToolGivenManagerFailureWhenSearchingThenReturnsSafeRecoveryCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		code   string
	}{
		{"turn not authorized", 403, `{"code":403,"message":"node-secret grant-secret"}`, "authorization_required"},
		{"envelope denied", 200, `{"code":403,"message":"node-secret grant-secret","data":{}}`, "authorization_required"},
		{"node rejected", 401, `{"code":401,"message":"node-secret"}`, "unauthenticated"},
		{"old Manager", 404, `{"code":404}`, "unavailable"},
		{"redirect", 302, `node-secret`, "unavailable"},
		{"invalid response", 200, `node-secret`, "invalid_response"},
		{"empty result", 200, `{"code":200,"data":null}`, "invalid_response"},
		{"oversized result", 200, strings.Repeat("x", secretaryToolMaxBytes+1), "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "node-secret").WorkspaceTool(t.Context(), "agent-1", WorkspaceToolRequest{SessionID: "caller-session", Tool: "search", ArgumentsJSON: `{"query":"cicd","scope":{"node_ids":["self"],"agent_ids":["*"],"session_ids":["*"]}}`})
			require.Error(t, err)
			code, message := WorkspaceToolFailure(err)
			assert.Equal(t, tc.code, code)
			assert.NotContains(t, message, "node-secret")
			assert.NotContains(t, message, "grant-secret")
			assert.Equal(t, 1, calls)
		})
	}
}

func TestWorkspaceToolGivenInvalidRequestWhenCalledThenDoesNotSend(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	client := NewClient(server.URL, "node-secret")
	for _, request := range []WorkspaceToolRequest{
		{Tool: "search", ArgumentsJSON: `{}`},
		{SessionID: "session-1", Tool: "read", ArgumentsJSON: `{}`},
		{SessionID: "session-1", Tool: "search", ArgumentsJSON: `null`},
		{SessionID: "session-1", Tool: "search", ArgumentsJSON: `[]`},
	} {
		_, err := client.WorkspaceTool(t.Context(), "agent-1", request)
		require.Error(t, err)
	}
	_, err := client.WorkspaceTool(t.Context(), "../agent", WorkspaceToolRequest{SessionID: "session-1", Tool: "search", ArgumentsJSON: `{}`})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.WorkspaceTool(ctx, "agent-1", WorkspaceToolRequest{SessionID: "session-1", Tool: "search", ArgumentsJSON: `{}`})
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, calls)
}

func TestWorkspaceToolGivenLifecycleOperationThenPreservesRequestAndResponse(t *testing.T) {
	for _, operation := range []string{"access_status", "cancel_access"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			response := `{"status":"cancelled","approval_id":"approval-1","turn_id":"turn-1","scope":{"queries":["*"],"node_ids":["*"],"agent_ids":["agent-target"],"session_ids":["*"]},"poll_after_ms":2000}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, "/api/v1/node/agents/agent-1/workspace/tools", r.URL.Path)
				assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
				assert.Empty(t, r.Header.Get("Authorization"))
				var body map[string]any
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Len(t, body, 3)
				assert.Equal(t, "caller-session", body["session_id"])
				assert.Equal(t, operation, body["tool"])
				assert.JSONEq(t, `{"approval_id":"approval-1"}`, body["arguments_json"].(string))
				_, _ = w.Write([]byte(`{"code":200,"data":` + response + `}`))
			}))
			defer server.Close()
			data, err := NewClient(server.URL, "node-secret").WorkspaceTool(t.Context(), "agent-1", WorkspaceToolRequest{SessionID: "caller-session", Tool: operation, ArgumentsJSON: `{"approval_id":"approval-1"}`})
			require.NoError(t, err)
			assert.JSONEq(t, response, string(data))
			assert.Equal(t, 1, calls)
		})
	}
}

func TestWorkspaceToolGivenLegacyScopeThenDoesNotSubstituteCallerNode(t *testing.T) {
	arguments := `{"query":"cicd","scope":{"agent_ids":["self"],"session_ids":["*"]}}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request WorkspaceToolRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "caller-session", request.SessionID)
		assert.Equal(t, "search", request.Tool)
		assert.JSONEq(t, arguments, request.ArgumentsJSON)
		assert.NotContains(t, request.ArgumentsJSON, "node_ids")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"message":"legacy all-nodes scope requires authorization"}`))
	}))
	defer server.Close()
	_, err := NewClient(server.URL, "node-secret").WorkspaceTool(t.Context(), "agent-1", WorkspaceToolRequest{
		SessionID: "caller-session", Tool: "search", ArgumentsJSON: arguments,
	})
	require.Error(t, err)
	code, message := WorkspaceToolFailure(err)
	assert.Equal(t, "authorization_required", code)
	assert.Contains(t, message, `"node_ids":["self"]`)
	assert.Contains(t, message, "workspace_request_access")
	assert.Equal(t, 1, calls)
}
