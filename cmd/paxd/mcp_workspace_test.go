package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceMCPGivenCapabilityWhenListingThenAddsOnlyReadOnlyTools(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "")
	require.Len(t, conversationMCPTools(), 8)
	_, err := callConversationMCPTool(context.Background(), json.RawMessage(`{"name":"workspace_read","arguments":{"ref":{"kind":"message","id":"m1"}}}`))
	require.Error(t, err)
	_, err = callConversationMCPTool(context.Background(), json.RawMessage(`{"name":"workspace_summaries","arguments":{"session_id":"source-session"}}`))
	require.Error(t, err)
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap")
	tools := conversationMCPTools()
	require.Len(t, tools, 5)
	for _, tool := range tools {
		schema := tool["inputSchema"].(map[string]any)
		assert.Equal(t, false, schema["additionalProperties"])
		annotations := tool["annotations"].(map[string]any)
		assert.Equal(t, true, annotations["readOnlyHint"])
	}
	assert.Equal(t, "workspace_history", tools[3]["name"])
	assert.Equal(t, "workspace_summaries", tools[4]["name"])
	for _, name := range []string{"list_agents", "ask", "reply", "publish_artifact"} {
		_, err := callConversationMCPTool(context.Background(), json.RawMessage(`{"name":"`+name+`","arguments":{}}`))
		require.Error(t, err)
		assert.Equal(t, "tool unavailable in Secretary execution", err.Error())
	}
}

func TestWorkspaceMCPGivenExpandWhenCalledThenUsesEnvironmentIdentityAndExactVersion(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap-secret")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	before := callSecretaryWorkspace
	t.Cleanup(func() { callSecretaryWorkspace = before })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callSecretaryWorkspace = func(ctx context.Context, agentID string, request cloud.SecretaryToolRequest) (json.RawMessage, error) {
		assert.NotNil(t, ctx.Done())
		assert.Equal(t, "agent_runtime", agentID)
		assert.Equal(t, "execution-session", request.SessionID)
		assert.Equal(t, "cap-secret", request.Capability)
		assert.Equal(t, "read", request.Tool)
		assert.JSONEq(t, `{"ref":{"kind":"message","id":"m1","content_hash":"frozen","canonicalization_version":"v1"},"view":"full","offset":0,"limit":16384}`, request.ArgumentsJSON)
		return json.RawMessage(`{"ref":{"id":"m1","content_hash":"frozen"},"text":"数据库"}`), nil
	}
	result, err := callConversationMCPTool(ctx, json.RawMessage(`{"name":"workspace_expand","arguments":{"message_id":"m1","content_hash":"frozen","canonicalization_version":"v1","view":"full","offset":0,"limit":16384}}`))
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.JSONEq(t, `{"ref":{"id":"m1","content_hash":"frozen"},"text":"数据库"}`, result.Content[0].Text)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "cap-secret")
	assert.Contains(t, string(raw), "structuredContent")
}

func TestWorkspaceMCPGivenUntrustedArgumentsWhenInvalidThenDoesNotCallManager(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	before := callSecretaryWorkspace
	t.Cleanup(func() { callSecretaryWorkspace = before })
	calls := 0
	callSecretaryWorkspace = func(context.Context, string, cloud.SecretaryToolRequest) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{}`), nil
	}
	cases := []struct{ name, tool, args string }{
		{"missing query", "search", `{}`},
		{"wrong type", "search", `{"query":123}`},
		{"unknown identity", "search", `{"query":"data","agent_id":"foreign"}`},
		{"unknown capability", "search", `{"query":"data","capability":"forged"}`},
		{"unknown owner", "search", `{"query":"data","owner_user_id":"foreign"}`},
		{"empty query", "search", `{"query":""}`},
		{"query too long", "search", `{"query":"` + strings.Repeat("数", 257) + `"}`},
		{"unknown kind", "search", `{"query":"data","kinds":["private"]}`},
		{"too many kinds", "search", `{"query":"data","kinds":["message","session","summary","message"]}`},
		{"fractional limit", "search", `{"query":"data","limit":1.5}`},
		{"zero search limit", "search", `{"query":"data","limit":0}`},
		{"too large search limit", "search", `{"query":"data","limit":51}`},
		{"session list value", "search", `{"query":"data","session_ids":[1]}`},
		{"null ref", "read", `{"ref":null}`},
		{"unknown ref field", "read", `{"ref":{"kind":"message","id":"m1","owner":"foreign"}}`},
		{"invalid ref kind", "read", `{"ref":{"kind":"private","id":"m1"}}`},
		{"overflow revision", "read", `{"ref":{"kind":"summary","id":"s1","revision":9223372036854775808}}`},
		{"negative offset", "read", `{"ref":{"kind":"message","id":"m1"},"offset":-1}`},
		{"too much context", "read", `{"ref":{"kind":"message","id":"m1"},"after":51}`},
		{"hidden view", "read", `{"ref":{"kind":"message","id":"m1"},"view":"reasoning"}`},
		{"unfrozen expand", "expand", `{"message_id":"m1","view":"full"}`},
		{"last expand", "expand", `{"message_id":"m1","content_hash":"h","canonicalization_version":"v1","view":"last"}`},
		{"missing summaries session", "summaries", `{}`},
		{"empty summaries session", "summaries", `{"session_id":""}`},
		{"summaries owner injection", "summaries", `{"session_id":"source","owner_user_id":"foreign"}`},
		{"summaries unknown field", "summaries", `{"session_id":"source","write":true}`},
		{"summaries empty cursor", "summaries", `{"session_id":"source","cursor":""}`},
		{"summaries negative limit", "summaries", `{"session_id":"source","limit":-1}`},
		{"summaries oversized limit", "summaries", `{"session_id":"source","limit":51}`},
		{"summaries fractional limit", "summaries", `{"session_id":"source","limit":1.5}`},
		{"missing history session", "history", `{}`},
		{"negative history cursor", "history", `{"session_id":"source","before_seq":-1}`},
		{"two history directions", "history", `{"session_id":"source","before_seq":1,"after_seq":2}`},
		{"history limit", "history", `{"session_id":"source","limit":51}`},
		{"empty history turn", "history", `{"session_id":"source","turn_id":""}`},
		{"invalid history turn type", "history", `{"session_id":"source","turn_id":1}`},
		{"history turn too long", "history", `{"session_id":"source","turn_id":"` + strings.Repeat("t", 4097) + `"}`},
		{"hidden history view", "history", `{"session_id":"source","view":"reasoning"}`},
		{"empty history view", "history", `{"session_id":"source","view":""}`},
		{"array input", "search", `[]`},
		{"null input", "search", `null`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"name":"workspace_` + tc.tool + `","arguments":` + tc.args + `}`
			_, err := callConversationMCPTool(context.Background(), json.RawMessage(raw))
			require.Error(t, err)
		})
	}
	assert.Zero(t, calls)
}

func TestWorkspaceMCPGivenHistoryWhenCalledThenTargetCannotReplaceCallerSession(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	before := callSecretaryWorkspace
	t.Cleanup(func() { callSecretaryWorkspace = before })
	callSecretaryWorkspace = func(ctx context.Context, agentID string, request cloud.SecretaryToolRequest) (json.RawMessage, error) {
		assert.Equal(t, "execution-session", request.SessionID)
		assert.Equal(t, "history", request.Tool)
		assert.JSONEq(t, `{"session_id":"source-session","after_seq":9007199254740993,"limit":0}`, request.ArgumentsJSON)
		return json.RawMessage(`{"messages":[],"pagination":{"has_more":false}}`), nil
	}
	result, err := callConversationMCPTool(context.Background(), json.RawMessage(`{"name":"workspace_history","arguments":{"session_id":"source-session","after_seq":9007199254740993,"limit":0}}`))
	require.NoError(t, err)
	assert.False(t, result.IsError)
}

func TestWorkspaceMCPGivenTurnHistoryWhenCalledThenPreservesOptionalViewAndCaller(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	before := callSecretaryWorkspace
	t.Cleanup(func() { callSecretaryWorkspace = before })
	for name, arguments := range map[string]string{
		"whole turn":   `{"session_id":"source-session","turn_id":"turn-1","view":"full","limit":50}`,
		"first view":   `{"session_id":"source-session","turn_id":"turn-1","view":"first"}`,
		"last view":    `{"session_id":"source-session","view":"last"}`,
		"compact turn": `{"session_id":"source-session","turn_id":"turn-1"}`,
		"defaults":     `{"session_id":"source-session"}`,
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			callSecretaryWorkspace = func(_ context.Context, agentID string, request cloud.SecretaryToolRequest) (json.RawMessage, error) {
				calls++
				assert.Equal(t, "agent_runtime", agentID)
				assert.Equal(t, "execution-session", request.SessionID)
				assert.Equal(t, "cap", request.Capability)
				assert.Equal(t, "history", request.Tool)
				assert.JSONEq(t, arguments, request.ArgumentsJSON)
				return json.RawMessage(`{"messages":[],"pagination":{"has_more":false}}`), nil
			}

			result, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_history","arguments":`+arguments+`}`))

			require.NoError(t, err)
			assert.False(t, result.IsError)
			assert.Equal(t, 1, calls)
		})
	}
}

func TestWorkspaceMCPGivenHistoryDiscoveryThenTurnAndViewAreOptional(t *testing.T) {
	for _, tool := range workspaceMCPTools() {
		if tool["name"] != "workspace_history" {
			continue
		}
		schema := tool["inputSchema"].(workspaceSchema)
		assert.Equal(t, []string{"session_id"}, schema["required"])
		properties := schema["properties"].(map[string]any)
		require.Contains(t, properties, "turn_id")
		require.Contains(t, properties, "view")
		assert.Equal(t, workspaceString(4096), properties["turn_id"])
		assert.Equal(t, workspaceEnum("first", "last", "full"), properties["view"])
		return
	}
	t.Fatal("workspace_history was not advertised")
}

func TestWorkspaceMCPGivenFailureWhenCalledThenReturnsOnlySafeErrors(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap-secret")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	before := callSecretaryWorkspace
	t.Cleanup(func() { callSecretaryWorkspace = before })
	for _, failure := range []error{errors.New("node-key-secret cap-secret arbitrary server body"), context.Canceled} {
		callSecretaryWorkspace = func(ctx context.Context, _ string, _ cloud.SecretaryToolRequest) (json.RawMessage, error) {
			return nil, failure
		}
		result, err := callConversationMCPTool(context.Background(), json.RawMessage(`{"name":"workspace_search","arguments":{"query":"data"}}`))
		require.NoError(t, err)
		assert.True(t, result.IsError)
		raw, _ := json.Marshal(result)
		assert.NotContains(t, string(raw), "node-key-secret")
		assert.NotContains(t, string(raw), "cap-secret")
	}
	t.Setenv("PAX_AGENT_ID", "")
	result, err := callConversationMCPTool(context.Background(), json.RawMessage(`{"name":"workspace_search","arguments":{"query":"data"}}`))
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

func TestWorkspaceMCPGivenStdioWhenReadingThenPreservesLargeRevision(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	before := callSecretaryWorkspace
	t.Cleanup(func() { callSecretaryWorkspace = before })
	callSecretaryWorkspace = func(ctx context.Context, _ string, request cloud.SecretaryToolRequest) (json.RawMessage, error) {
		assert.Contains(t, request.ArgumentsJSON, "9007199254740993")
		return json.RawMessage(`{"revision":9007199254740993}`), nil
	}
	var output bytes.Buffer
	require.NoError(t, serveConversationMCP(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workspace_read","arguments":{"ref":{"kind":"summary","id":"s1","revision":9007199254740993}}}}`), &output))
	assert.Contains(t, output.String(), `"structuredContent":{"revision":9007199254740993}`)
}

func TestWorkspaceMCPGivenStoredNodeCredentialsWhenStdioReadsThenNeedsNoUserKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAXD_DB_PATH", filepath.Join(home, ".paxd", "paxd.db"))
	t.Setenv("PAX_SECRETARY_CAPABILITY", "execution-capability")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	t.Setenv("PAX_USER_API_KEY", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/node/agents/agent_runtime/secretary/tools", r.URL.Path)
		assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
		assert.Empty(t, r.Header.Get("Authorization"))
		var request cloud.SecretaryToolRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		assert.Equal(t, "execution-session", request.SessionID)
		assert.Equal(t, "execution-capability", request.Capability)
		assert.Equal(t, "read", request.Tool)
		assert.JSONEq(t, `{"ref":{"kind":"message","id":"m1","content_hash":"frozen","canonicalization_version":"v1"}}`, request.ArgumentsJSON)
		_, _ = w.Write([]byte(`{"code":200,"data":{"ref":{"kind":"message","id":"m1","content_hash":"frozen","canonicalization_version":"v1"},"text":"数据库","read_receipt":"exact-receipt"}}`))
	}))
	defer server.Close()
	seedConversationRemote(t, home, server.URL, "agent_runtime")
	var output bytes.Buffer
	require.NoError(t, serveConversationMCP(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"workspace_read","arguments":{"ref":{"kind":"message","id":"m1","content_hash":"frozen","canonicalization_version":"v1"}}}}`), &output))
	assert.Contains(t, output.String(), "exact-receipt")
	assert.NotContains(t, output.String(), "node-secret")
	assert.NotContains(t, output.String(), "execution-capability")
}

func TestWorkspaceMCPGivenSummaryPaginationWhenCalledThenPreservesOriginAndExactRevisions(t *testing.T) {
	t.Setenv("PAX_SECRETARY_CAPABILITY", "cap")
	t.Setenv("PAX_AGENT_ID", "agent_runtime")
	t.Setenv("PAX_SESSION_ID", "execution-session")
	before := callSecretaryWorkspace
	t.Cleanup(func() { callSecretaryWorkspace = before })
	for name, arguments := range map[string]string{
		"defaults":     `{"session_id":"origin-session"}`,
		"continuation": `{"session_id":"origin-session","cursor":"opaque+next","limit":50}`,
		"zero default": `{"session_id":"origin-session","limit":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			callSecretaryWorkspace = func(_ context.Context, agentID string, request cloud.SecretaryToolRequest) (json.RawMessage, error) {
				assert.Equal(t, "agent_runtime", agentID)
				assert.Equal(t, "execution-session", request.SessionID)
				assert.Equal(t, "summaries", request.Tool)
				assert.JSONEq(t, arguments, request.ArgumentsJSON)
				return json.RawMessage(`{"items":[{"summary_id":"node-1","revision":9007199254740993,"title":"Search","body":"Use ES"},{"summary_id":"node-2","revision":1,"title":"Deployment","body":"Docker"}],"next_cursor":"opaque+next"}`), nil
			}
			result, err := callConversationMCPTool(t.Context(), json.RawMessage(`{"name":"workspace_summaries","arguments":`+arguments+`}`))
			require.NoError(t, err)
			assert.False(t, result.IsError)
			assert.Contains(t, result.Content[0].Text, `"revision":9007199254740993`)
			assert.Contains(t, result.Content[0].Text, `"next_cursor":"opaque+next"`)
		})
	}
}

func TestWorkspaceMCPGivenSummaryDiscoveryThenHasBoundedReadOnlyPagination(t *testing.T) {
	for _, tool := range workspaceMCPTools() {
		if tool["name"] != "workspace_summaries" {
			continue
		}
		schema := tool["inputSchema"].(workspaceSchema)
		assert.Equal(t, []string{"session_id"}, schema["required"])
		properties := schema["properties"].(map[string]any)
		assert.Equal(t, workspaceString(4096), properties["session_id"])
		assert.Equal(t, workspaceString(16384), properties["cursor"])
		limit := properties["limit"].(workspaceSchema)
		assert.Equal(t, int64(0), limit["minimum"])
		assert.Equal(t, int64(50), limit["maximum"])
		assert.Equal(t, 20, limit["default"])
		return
	}
	t.Fatal("workspace_summaries was not advertised")
}
