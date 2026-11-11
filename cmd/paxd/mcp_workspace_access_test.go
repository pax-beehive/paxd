package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrdinaryWorkspaceMCPGivenNoSecretaryCapabilityWhenListedThenExposesSearchAndAccess(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	tools := conversationMCPTools()
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool["name"].(string))
	}
	assert.ElementsMatch(t, []string{"list_agents", "ask", "reply", "publish_artifact", "workspace_search", "workspace_request_access"}, names)
	for _, tool := range ordinaryWorkspaceMCPTools() {
		annotations := tool["annotations"].(map[string]any)
		assert.Equal(t, tool["name"] == "workspace_search", annotations["readOnlyHint"])
		if tool["name"] == "workspace_search" {
			schema := tool["inputSchema"].(workspaceSchema)
			assert.ElementsMatch(t, []string{"query", "scope"}, schema["required"])
			assert.NotContains(t, schema["properties"], "session_ids")
		}
	}
	t.Setenv(envPaxSecretaryCapability, "secretary-capability")
	secretarySchema := conversationMCPTools()[0]["inputSchema"].(workspaceSchema)
	assert.Equal(t, []string{"query"}, secretarySchema["required"])
	assert.Contains(t, secretarySchema["properties"], "session_ids")
	assert.NotContains(t, secretarySchema["properties"], "scope")
	_, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_request_access","arguments":{}}`))
	require.ErrorContains(t, err, "tool unavailable in Secretary execution")
}

func TestOrdinaryWorkspaceMCPGivenAccessRequestWhenPendingThenReturnsWithoutWaiting(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	t.Setenv(envPaxAgentID, "agent-caller")
	t.Setenv(envPaxSessionID, "caller-session")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	callOrdinaryWorkspace = func(_ context.Context, agentID string, request cloud.WorkspaceToolRequest) (json.RawMessage, error) {
		assert.Equal(t, "agent-caller", agentID)
		assert.Equal(t, "caller-session", request.SessionID)
		assert.Equal(t, "request_access", request.Tool)
		assert.JSONEq(t, `{"queries":["*"],"agent_ids":["*"],"session_ids":["*"],"reason":"Find prior CI decisions"}`, request.ArgumentsJSON)
		return json.RawMessage(`{"status":"pending","approval_id":"approval-1","turn_id":"turn-1","scope":{"queries":["*"],"agent_ids":["*"],"session_ids":["*"]}}`), nil
	}
	result, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_request_access","arguments":{"reason":"Find prior CI decisions"}}`))
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.JSONEq(t, `{"status":"pending","approval_id":"approval-1","turn_id":"turn-1","scope":{"queries":["*"],"agent_ids":["*"],"session_ids":["*"]}}`, result.Content[0].Text)
}

func TestOrdinaryWorkspaceMCPGivenExplicitScopeWhenRequestedThenPreservesExactQueries(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	t.Setenv(envPaxAgentID, "agent-caller")
	t.Setenv(envPaxSessionID, "caller-session")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	callOrdinaryWorkspace = func(_ context.Context, _ string, request cloud.WorkspaceToolRequest) (json.RawMessage, error) {
		assert.JSONEq(t, `{"queries":["CI CD","release?"],"agent_ids":["self","agent-target"],"session_ids":["session-1","session-2"]}`, request.ArgumentsJSON)
		return json.RawMessage(`{"status":"approved","approval_id":"approval-1","turn_id":"turn-1"}`), nil
	}
	_, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_request_access","arguments":{"queries":["CI CD","release?"],"agent_ids":["self","agent-target"],"session_ids":["session-1","session-2"]}}`))
	require.NoError(t, err)
}

func TestOrdinaryWorkspaceMCPGivenUntrustedScopeWhenInvalidThenDoesNotSend(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	t.Setenv(envPaxAgentID, "agent-caller")
	t.Setenv(envPaxSessionID, "caller-session")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	callOrdinaryWorkspace = func(context.Context, string, cloud.WorkspaceToolRequest) (json.RawMessage, error) {
		t.Fatal("invalid arguments reached Manager")
		return nil, nil
	}
	for _, args := range []string{
		`{"queries":[]}`, `{"session_ids":[]}`, `{"queries":["*","cicd"]}`, `{"session_ids":["session-1","*"]}`,
		`{"agent_ids":[]}`, `{"agent_ids":["*","self"]}`, `{"agent_ids":[" "]}`, `{"agent_ids":[1]}`,
		`{"queries":[" "]}`, `{"queries":[1]}`, `{"session_ids":[""]}`, `{"reason":123}`,
		`{"queries":["` + strings.Repeat("q", 257) + `"]}`, `{"owner_user_id":"foreign"}`,
		`{"turn_id":"forged"}`, `{"session_id":"forged"}`, `{"capability":"forged"}`, `{"grant_id":"forged"}`,
	} {
		t.Run(args, func(t *testing.T) {
			_, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_request_access","arguments":`+args+`}`))
			require.Error(t, err)
		})
	}
	for _, args := range []string{`{"query":"cicd","turn_id":"forged"}`, `{"query":"cicd","owner_user_id":"foreign"}`, `{"query":"cicd","capability":"forged"}`} {
		_, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_search","arguments":`+args+`}`))
		require.Error(t, err)
	}
}

func TestOrdinaryWorkspaceMCPGivenMissingOrInvalidSearchScopeThenReturnsExplicitExampleWithoutSending(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	callOrdinaryWorkspace = func(context.Context, string, cloud.WorkspaceToolRequest) (json.RawMessage, error) {
		t.Fatal("invalid search scope reached Manager")
		return nil, nil
	}
	for _, args := range []string{
		`{"query":"cicd"}`,
		`{"query":"cicd","scope":{}}`,
		`{"query":"cicd","scope":{"agent_ids":["self"]}}`,
		`{"query":"cicd","scope":{"session_ids":["*"]}}`,
		`{"query":"cicd","scope":{"agent_ids":[],"session_ids":["*"]}}`,
		`{"query":"cicd","scope":{"agent_ids":["self"],"session_ids":[]}}`,
		`{"query":"cicd","scope":{"agent_ids":["*","self"],"session_ids":["*"]}}`,
		`{"query":"cicd","scope":{"agent_ids":["self"],"session_ids":["*","session-1"]}}`,
		`{"query":"cicd","scope":{"agent_ids":[" * ","self"],"session_ids":["*"]}}`,
		`{"query":"cicd","scope":{"agent_ids":[" "],"session_ids":["*"]}}`,
		`{"query":"cicd","scope":{"agent_ids":["self"],"session_ids":[" "]}}`,
		`{"query":"cicd","scope":null}`,
		`{"query":"cicd","scope":{"agent_ids":["self"],"session_ids":["*"],"owner":"foreign"}}`,
		`{"query":"cicd","scope":{"agent_ids":["self"],"session_ids":["*"]},"session_ids":["old-top-level"]}`,
	} {
		t.Run(args, func(t *testing.T) {
			_, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_search","arguments":`+args+`}`))
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"agent_ids":["self"]`)
			assert.Contains(t, err.Error(), `"session_ids":["*"]`)
			assert.Contains(t, err.Error(), "workspace_request_access")
		})
	}
}

func TestOrdinaryWorkspaceMCPGivenExplicitSearchScopeThenPreservesIntentAndResolvedScope(t *testing.T) {
	t.Setenv(envPaxSecretaryCapability, "")
	t.Setenv(envPaxAgentID, "agent-caller")
	t.Setenv(envPaxSessionID, "caller-session")
	before := callOrdinaryWorkspace
	t.Cleanup(func() { callOrdinaryWorkspace = before })
	for _, scope := range []string{
		`{"agent_ids":["self"],"session_ids":["*"]}`,
		`{"agent_ids":["*"],"session_ids":["session-1"]}`,
		`{"agent_ids":["self","agent-target"],"session_ids":["session-1","session-2"]}`,
	} {
		callOrdinaryWorkspace = func(_ context.Context, agentID string, request cloud.WorkspaceToolRequest) (json.RawMessage, error) {
			assert.Equal(t, "agent-caller", agentID)
			assert.Equal(t, "caller-session", request.SessionID)
			assert.Equal(t, "search", request.Tool)
			assert.JSONEq(t, `{"query":"cicd","scope":`+scope+`}`, request.ArgumentsJSON)
			return json.RawMessage(`{"hits":[],"scope":{"agent_ids":["agent-caller","agent-target"],"session_ids":["session-1"]}}`), nil
		}
		result, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_search","arguments":{"query":"cicd","scope":`+scope+`}}`))
		require.NoError(t, err)
		assert.False(t, result.IsError)
		assert.JSONEq(t, `{"hits":[],"scope":{"agent_ids":["agent-caller","agent-target"],"session_ids":["session-1"]}}`, result.Content[0].Text)
	}
}

func TestOrdinaryWorkspaceMCPGivenTurnAuthorizationRevokedWhenSearchingThenDoesNotCacheOrRequestAccess(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAXD_DB_PATH", filepath.Join(home, ".paxd", "paxd.db"))
	t.Setenv(envPaxSecretaryCapability, "")
	t.Setenv(envPaxAgentID, "agent_runtime")
	t.Setenv(envPaxSessionID, "caller-session")
	t.Setenv("PAX_USER_API_KEY", "")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		assert.Equal(t, "/api/v1/node/agents/agent_runtime/workspace/tools", r.URL.Path)
		assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
		assert.Empty(t, r.Header.Get("Authorization"))
		var request cloud.WorkspaceToolRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "caller-session", request.SessionID)
		assert.Equal(t, "search", request.Tool)
		assert.JSONEq(t, `{"query":"cicd","scope":{"agent_ids":["*"],"session_ids":["*"]},"limit":10}`, request.ArgumentsJSON)
		if calls == 1 {
			_, _ = w.Write([]byte(`{"code":200,"data":{"hits":[{"ref":{"kind":"message","id":"m1"},"snippet":"prior CI decision"}]}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":403,"message":"node-secret revoked turn grant"}`))
	}))
	defer server.Close()
	seedConversationRemote(t, home, server.URL, "agent_runtime")
	params := json.RawMessage(`{"name":"workspace_search","arguments":{"query":"cicd","scope":{"agent_ids":["*"],"session_ids":["*"]},"limit":10}}`)
	first, err := callConversationMCPTool(t.Context(), params)
	require.NoError(t, err)
	assert.False(t, first.IsError)
	second, err := callConversationMCPTool(t.Context(), params)
	require.NoError(t, err)
	assert.True(t, second.IsError)
	raw, err := json.Marshal(second)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "authorization_required")
	assert.Contains(t, second.Content[0].Text, "Default permission covers all sessions of this agent")
	assert.Contains(t, second.Content[0].Text, "workspace_request_access")
	assert.NotContains(t, string(raw), "node-secret")
	assert.Equal(t, 2, calls, "search must not create an access request or retry after revocation")
}
