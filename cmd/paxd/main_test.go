package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/remotelogin"
	"github.com/pax-beehive/paxd/internal/remotesecrets"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestAppExposesDaemonLoginSetupAndServiceCommands(t *testing.T) {
	app := newApp()

	names := make([]string, 0, len(app.Commands))
	for _, command := range app.Commands {
		names = append(names, command.Name)
	}

	assert.ElementsMatch(t, []string{"setup", "login", "update", "mcp", "run", "service"}, names)
	assert.NotContains(t, names, "connect")
	assert.NotContains(t, names, "configure")
	assert.NotContains(t, names, "register")
	assert.NotContains(t, names, "acp-forward")
	assert.NotContains(t, names, "postman")
	assert.NotContains(t, names, "harnesses")
	assert.NotContains(t, names, "install-service")
}

func TestPaxdLoginCommitsRemoteAndLocalSecretRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	restore := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		assert.Equal(t, "prod", spec.RemoteID)
		assert.Equal(t, "https://api.example.test", spec.CloudAPIURL)
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_123",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restore()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"login",
		"--remote", "prod",
		"--cloud-url", "https://api.example.test",
	})
	require.NoError(t, err)

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	remotes, err := store.ListRemotes(context.Background(), control.ListRemotesQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, remotes, 1)

	secretPath := filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "node_key")
	assert.Equal(t, "prod", remotes[0].Remote.ID)
	assert.Equal(t, "prod", remotes[0].Remote.Name)
	assert.Equal(t, "https://api.example.test", remotes[0].Remote.CloudAPIURL)
	assert.Equal(t, "node_123", remotes[0].Remote.NodeID)
	material, err := store.GetRemoteAuthMaterial(context.Background(), "prod")
	require.NoError(t, err)
	assert.Equal(t, "file:"+secretPath, material.CloudAPIKeyRef)
	assert.NotContains(t, material.CloudAPIKeyRef, "node-secret")
	assert.FileExists(t, secretPath)
}

func TestPaxdLoginPersistsCloudflareAccessAuthFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_CLOUD_CF_CLIENT_ID", "cf-client")
	t.Setenv("PAX_CLOUD_CF_CLIENT_SECRET", "cf-secret")
	restore := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_123",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restore()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"login",
		"--remote", "prod",
		"--cloud-url", "https://api.example.test",
	})
	require.NoError(t, err)

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	material, err := store.GetRemoteAuthMaterial(context.Background(), "prod")
	require.NoError(t, err)
	require.NotNil(t, material.CloudflareAccess)
	secretPath := filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "cf_access_client_secret")
	assert.Equal(t, control.RemoteAuthCloudflareAccess, material.AuthKind)
	assert.Equal(t, "cf-client", material.CloudflareAccess.ClientID)
	assert.Equal(t, "file:"+secretPath, material.CloudflareAccess.ClientSecretRef)
	assert.NotContains(t, material.CloudflareAccess.ClientSecretRef, "cf-secret")
	data, err := os.ReadFile(secretPath)
	require.NoError(t, err)
	assert.Equal(t, "cf-secret\n", string(data))
}

func TestPaxdLoginDoesNotExposeConfigFlag(t *testing.T) {
	cmd := cmdLoginCommand()

	names := flagNames(cmd.Flags)
	assert.Contains(t, names, "remote")
	assert.Contains(t, names, "cloud-url")
	assert.Contains(t, names, "api-endpoint")
	assert.NotContains(t, names, "config")
}

func TestPaxdLoginDoesNotCommitOnLoginFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	restore := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{}, errors.New("approval denied")
	})
	defer restore()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"login",
		"--remote", "prod",
		"--cloud-url", "https://api.example.test",
	})
	require.Error(t, err)

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	remotes, err := store.ListRemotes(context.Background(), control.ListRemotesQuery{IncludeDisabled: true})
	require.NoError(t, err)
	assert.Empty(t, remotes)
	assert.NoFileExists(t, filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "node_key"))
}

func TestCommitRemoteLoginUpdatesExistingRemote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := configWithDB(filepath.Join(home, ".paxd", "paxd.db"))
	store := openTestDaemonStore(t, cfg.Daemon.DBPath)
	enabled := true
	_, err := store.CreateRemote(context.Background(), control.CreateRemoteCommand{
		Remote: control.Remote{
			ID:          "prod",
			Name:        "Production",
			CloudAPIURL: "https://old.example.test",
			NodeID:      "node_old",
			Enabled:     &enabled,
		},
		CloudAPIKeyRef: "file:/old/key",
	})
	require.NoError(t, err)

	err = commitRemoteLogin(context.Background(), cfg, remotelogin.LoginResult{
		RemoteID:    "prod",
		CloudAPIURL: "https://api.example.test",
		NodeID:      "node_new",
		NodeAPIKey:  "new-secret",
	})
	require.NoError(t, err)

	remotes, err := store.ListRemotes(context.Background(), control.ListRemotesQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, remotes, 1)
	assert.Equal(t, "Production", remotes[0].Remote.Name)
	assert.Equal(t, "https://api.example.test", remotes[0].Remote.CloudAPIURL)
	assert.Equal(t, "node_new", remotes[0].Remote.NodeID)

	material, err := store.GetRemoteAuthMaterial(context.Background(), "prod")
	require.NoError(t, err)
	assert.Equal(t, "file:"+filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "node_key"), material.CloudAPIKeyRef)
}

func TestCommitRemoteLoginRejectsMissingRemoteID(t *testing.T) {
	cfg := configWithDB(filepath.Join(t.TempDir(), "paxd.db"))

	err := commitRemoteLogin(context.Background(), cfg, remotelogin.LoginResult{
		NodeID:     "node_123",
		NodeAPIKey: "node-secret",
	})

	require.Error(t, err)
}

func TestCommandWriterFirstNonEmptyAndVerifyCancel(t *testing.T) {
	assert.NotNil(t, commandWriter(nil))
	assert.Equal(t, "value", firstNonEmpty("", " ", "value"))
	assert.Empty(t, firstNonEmpty("", " "))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := verifyLocalAPI(ctx, time.Nanosecond)
	require.ErrorIs(t, err, context.Canceled)
}

func TestMCPConversationAskPostsRepresentativeDelivery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_AGENT_ID", "agent_runtime_1")
	t.Setenv("PAX_REPRESENTATIVE_AGENT_ID", "rep_source")
	t.Setenv("PAX_SESSION_ID", "sess_1")
	inputPath := filepath.Join(home, "question.md")
	require.NoError(t, os.WriteFile(inputPath, []byte("question from file\n"), 0644))

	var gotBody cloud.ConversationDeliveryRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/node/conversation/deliver", r.URL.EscapedPath())
		assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"contract_version":"conversation_delivery.v1","receipt_token":"rcpt_1","delivery":{"delivery_status":"stored","receipt_token":"rcpt_1","conversation":{"conversation_id":"conv_1"},"invocation":{"invocation_id":"inv_1","target_session_id":"sess_target"},"prompt_message":{"message_id":"msg_1"}}}}`))
	}))
	defer server.Close()
	seedConversationRemote(t, home, server.URL, "agent_runtime_1")

	app := newApp()
	var stdout bytes.Buffer
	app.Writer = &stdout
	err := app.Run(context.Background(), []string{
		"paxd",
		"mcp",
		"conversation",
		"ask",
		"--to-representative-agent-id", "rep_target",
		"--input-file", inputPath,
		"--include-message",
	})

	require.NoError(t, err)
	require.NotNil(t, gotBody.Source)
	assert.Equal(t, "agent_runtime_1", gotBody.Source.AgentID)
	assert.Equal(t, "rep_source", gotBody.Source.RepresentativeAgentID)
	assert.Equal(t, "sess_1", gotBody.Source.SessionID)
	assert.Equal(t, "representative", gotBody.Target.Kind)
	assert.Equal(t, "rep_target", gotBody.Target.RepresentativeAgentID)
	assert.Empty(t, gotBody.Target.InvocationID)
	assert.True(t, gotBody.Context.LatestResponse)
	assert.Equal(t, "question from file\n", gotBody.Instruction)
	assert.Equal(t, "sent receipt=rcpt_1 target_session=sess_target\n", stdout.String())
}

func TestMCPConversationReplyPostsActiveInquiryTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_AGENT_ID", "agent_runtime_1")
	t.Setenv("PAX_REPRESENTATIVE_AGENT_ID", "rep_source")
	t.Setenv("PAX_SESSION_ID", "sess_1")

	var gotBody cloud.ConversationDeliveryRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"contract_version":"conversation_delivery.v1","delivery":{"delivery_status":"stored"}}}`))
	}))
	defer server.Close()
	seedConversationRemote(t, home, server.URL, "agent_runtime_1")

	app := newApp()
	var stdout bytes.Buffer
	app.Writer = &stdout
	err := app.Run(context.Background(), []string{
		"paxd",
		"mcp",
		"conversation",
		"reply",
		"wozhidaodaan",
	})

	require.NoError(t, err)
	require.NotNil(t, gotBody.Source)
	assert.Equal(t, "agent_runtime_1", gotBody.Source.AgentID)
	assert.Equal(t, "rep_source", gotBody.Source.RepresentativeAgentID)
	assert.Equal(t, "sess_1", gotBody.Source.SessionID)
	assert.Equal(t, "active_invocation", gotBody.Target.Kind)
	assert.Empty(t, gotBody.Target.InvocationID)
	assert.Empty(t, gotBody.Target.RepresentativeAgentID)
	assert.False(t, gotBody.Context.LatestResponse)
	assert.Equal(t, "wozhidaodaan", gotBody.Instruction)
	assert.Equal(t, "sent\n", stdout.String())
}

func TestConversationDeliverySummaryIncludesReceiptWhenPresent(t *testing.T) {
	got := conversationDeliverySummary(&cloud.ConversationDeliveryResponse{
		ReceiptToken: "rcpt_1",
		Delivery: json.RawMessage(`{
			"delivery_status":"stored",
			"conversation":{"conversation_id":"conv_1"},
			"invocation":{"invocation_id":"inv_1","target_session_id":"sess_target"},
			"prompt_message":{"message_id":"msg_1"}
		}`),
	})

	assert.Equal(t, "sent receipt=rcpt_1 target_session=sess_target", got)
}

func TestMCPConversationAskJSONPrintsFullResponse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_AGENT_ID", "agent_runtime_1")
	t.Setenv("PAX_SESSION_ID", "sess_1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"contract_version":"conversation_delivery.v1","delivery_endpoint":"/api/v1/node/conversation/deliver","delivery":{"delivery_status":"stored","instruction":"nihao"}}}`))
	}))
	defer server.Close()
	seedConversationRemote(t, home, server.URL, "agent_runtime_1")

	app := newApp()
	var stdout bytes.Buffer
	app.Writer = &stdout
	err := app.Run(context.Background(), []string{
		"paxd",
		"mcp",
		"conversation",
		"ask",
		"--to-representative-agent-id", "rep_target",
		"--json",
		"nihao",
	})

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), `"contract_version": "conversation_delivery.v1"`)
	assert.Contains(t, stdout.String(), `"delivery_endpoint": "/api/v1/node/conversation/deliver"`)
	assert.Contains(t, stdout.String(), `"instruction": "nihao"`)
}

func TestMCPConversationReplyRequiresSessionEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAX_AGENT_ID", "agent_runtime_1")

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"mcp",
		"conversation",
		"reply",
		"answer",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "PAX_SESSION_ID")
}

func TestMCPConversationServeGivenToolsListWhenCalledThenReturnsConversationAndArtifactTools(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
	}, "\n")
	var output bytes.Buffer

	err := serveConversationMCP(context.Background(), strings.NewReader(input), &output)

	require.NoError(t, err)
	responses := decodeMCPResponses(t, output.String())
	require.Len(t, responses, 2)
	assert.Equal(t, "pax-conversation", responses[0]["result"].(map[string]any)["serverInfo"].(map[string]any)["name"])
	tools := responses[1]["result"].(map[string]any)["tools"].([]any)
	require.Len(t, tools, 3)
	assert.Equal(t, "ask", tools[0].(map[string]any)["name"])
	assert.Equal(t, "reply", tools[1].(map[string]any)["name"])
	artifactTool := tools[2].(map[string]any)
	assert.Equal(t, "publish_artifact", artifactTool["name"])
	schema := artifactTool["inputSchema"].(map[string]any)
	assert.Equal(t, []any{"path"}, schema["required"])
}

func TestMCPConversationServeGivenPublishArtifactWhenDaemonAcceptsThenReturnsPublicationMarker(t *testing.T) {
	t.Setenv("PAX_AGENT_ID", "agent_runtime_1")
	t.Setenv("PAX_SESSION_ID", "sess_1")
	previous := publishArtifactThroughDaemon
	t.Cleanup(func() { publishArtifactThroughDaemon = previous })
	publishArtifactThroughDaemon = func(
		_ context.Context,
		req control.PublishArtifactRequest,
	) (control.ArtifactPublication, error) {
		assert.Equal(t, "agent_runtime_1", req.AgentID)
		assert.Equal(t, "sess_1", req.SessionID)
		assert.Equal(t, "/workspace/report.pdf", req.SourcePath)
		assert.Equal(t, "Analysis report", req.Title)
		return control.ArtifactPublication{
			PublicationID: "apub_1",
			Status:        "accepted",
		}, nil
	}
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"publish_artifact","arguments":{"path":"/workspace/report.pdf","title":"Analysis report"}}}`
	var output bytes.Buffer

	err := serveConversationMCP(context.Background(), strings.NewReader(input), &output)

	require.NoError(t, err)
	responses := decodeMCPResponses(t, output.String())
	require.Len(t, responses, 1)
	result := responses[0]["result"].(map[string]any)
	assert.Nil(t, result["isError"])
	content := result["content"].([]any)
	assert.JSONEq(
		t,
		`{"accepted":true,"pax_artifact_publication":{"publication_id":"apub_1"}}`,
		content[0].(map[string]any)["text"].(string),
	)
}

func TestMCPConversationServeGivenAskToolCallWhenCalledThenPostsRepresentativeDelivery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_AGENT_ID", "agent_runtime_1")
	t.Setenv("PAX_REPRESENTATIVE_AGENT_ID", "rep_source")
	t.Setenv("PAX_SESSION_ID", "sess_1")

	var gotBody cloud.ConversationDeliveryRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/node/conversation/deliver", r.URL.EscapedPath())
		assert.Equal(t, "node-secret", r.Header.Get("X-Pax-Key"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"contract_version":"conversation_delivery.v1","receipt_token":"rcpt_1","delivery":{"delivery_status":"stored","invocation":{"target_session_id":"sess_target"}}}}`))
	}))
	defer server.Close()
	seedConversationRemote(t, home, server.URL, "agent_runtime_1")
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask","arguments":{"to_representative_agent_id":"rep_target","text":"nihao","include_message":true}}}`
	var output bytes.Buffer

	err := serveConversationMCP(context.Background(), strings.NewReader(input), &output)

	require.NoError(t, err)
	require.NotNil(t, gotBody.Source)
	assert.Equal(t, "agent_runtime_1", gotBody.Source.AgentID)
	assert.Equal(t, "rep_source", gotBody.Source.RepresentativeAgentID)
	assert.Equal(t, "sess_1", gotBody.Source.SessionID)
	assert.Equal(t, "representative", gotBody.Target.Kind)
	assert.Equal(t, "rep_target", gotBody.Target.RepresentativeAgentID)
	assert.True(t, gotBody.Context.LatestResponse)
	assert.Equal(t, "nihao", gotBody.Instruction)
	responses := decodeMCPResponses(t, output.String())
	require.Len(t, responses, 1)
	result := responses[0]["result"].(map[string]any)
	content := result["content"].([]any)
	assert.Equal(t, "sent receipt=rcpt_1 target_session=sess_target", content[0].(map[string]any)["text"])
	assert.Nil(t, result["isError"])
}

func TestMCPConversationServeGivenReplyToolCallWhenCalledThenPostsActiveInvocationDelivery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_AGENT_ID", "agent_runtime_1")
	t.Setenv("PAX_REPRESENTATIVE_AGENT_ID", "rep_source")
	t.Setenv("PAX_SESSION_ID", "sess_1")

	var gotBody cloud.ConversationDeliveryRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"contract_version":"conversation_delivery.v1","delivery":{"delivery_status":"stored"}}}`))
	}))
	defer server.Close()
	seedConversationRemote(t, home, server.URL, "agent_runtime_1")
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"reply","arguments":{"text":"wozhidaodaan"}}}`
	var output bytes.Buffer

	err := serveConversationMCP(context.Background(), strings.NewReader(input), &output)

	require.NoError(t, err)
	require.NotNil(t, gotBody.Source)
	assert.Equal(t, "agent_runtime_1", gotBody.Source.AgentID)
	assert.Equal(t, "rep_source", gotBody.Source.RepresentativeAgentID)
	assert.Equal(t, "sess_1", gotBody.Source.SessionID)
	assert.Equal(t, "active_invocation", gotBody.Target.Kind)
	assert.Equal(t, "wozhidaodaan", gotBody.Instruction)
	responses := decodeMCPResponses(t, output.String())
	require.Len(t, responses, 1)
	result := responses[0]["result"].(map[string]any)
	content := result["content"].([]any)
	assert.Equal(t, "sent", content[0].(map[string]any)["text"])
}

func decodeMCPResponses(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	responses := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var response map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &response))
		responses = append(responses, response)
	}
	return responses
}

func TestPaxdSetupLogsInInstallsStartsAndVerifies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_CLOUD_CF_CLIENT_ID", "cf-client")
	t.Setenv("PAX_CLOUD_CF_CLIENT_SECRET", "cf-secret")
	var calls []string
	restoreLogin := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		calls = append(calls, "login:"+spec.RemoteID)
		assert.Equal(t, "default", spec.RemoteID)
		assert.Equal(t, config.DefaultCloudAPIURL, spec.CloudAPIURL)
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_123",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restoreLogin()
	restoreService := stubServiceOps(t,
		func(opts serviceInstallOptions) error {
			calls = append(calls, "service install")
			assert.True(t, opts.Force)
			assert.True(t, opts.SuppressNextSteps)
			return nil
		},
		func(action string, system bool) error {
			calls = append(calls, "service "+action)
			assert.False(t, system)
			return nil
		},
	)
	defer restoreService()
	restoreVerify := stubVerifyLocalAPI(t, func(ctx context.Context, timeout time.Duration) error {
		calls = append(calls, "verify")
		assert.Equal(t, 20*time.Second, timeout)
		return nil
	})
	defer restoreVerify()

	app := newApp()
	var stdout bytes.Buffer
	app.Writer = &stdout
	err := app.Run(context.Background(), []string{
		"paxd",
		"setup",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"login:default", "service install", "service restart", "verify"}, calls)
	assert.Contains(t, stdout.String(), "Connected default remote as node_123.")
	assert.Contains(t, stdout.String(), "Background service started.")
	assert.NotContains(t, stdout.String(), "Next steps:")
	assert.NotContains(t, stdout.String(), "paxd service start")
	assert.DirExists(t, filepath.Join(home, ".paxd"))

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	material, err := store.GetRemoteAuthMaterial(context.Background(), "default")
	require.NoError(t, err)
	require.NotNil(t, material.CloudflareAccess)
	secretPath := filepath.Join(home, ".paxd", "secrets", "remotes", "default", "cf_access_client_secret")
	assert.Equal(t, control.RemoteAuthCloudflareAccess, material.AuthKind)
	assert.Equal(t, "cf-client", material.CloudflareAccess.ClientID)
	assert.Equal(t, "file:"+secretPath, material.CloudflareAccess.ClientSecretRef)
	data, err := os.ReadFile(secretPath)
	require.NoError(t, err)
	assert.Equal(t, "cf-secret\n", string(data))
}

func TestPaxdSetupExposesOnlyCloudURLFlagWithDefault(t *testing.T) {
	cmd := cmdSetupCommand()

	require.Len(t, cmd.Flags, 1)
	flag, ok := cmd.Flags[0].(*cli.StringFlag)
	require.True(t, ok)
	assert.Equal(t, "cloud-url", flag.Names()[0])
	assert.Equal(t, config.DefaultCloudAPIURL, flag.Value)
}

func TestPaxdSetupDoesNotStartServiceWhenLoginFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	restoreLogin := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{}, errors.New("approval denied")
	})
	defer restoreLogin()
	serviceCalled := false
	restoreService := stubServiceOps(t,
		func(opts serviceInstallOptions) error {
			serviceCalled = true
			return nil
		},
		func(action string, system bool) error {
			serviceCalled = true
			return nil
		},
	)
	defer restoreService()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"setup",
		"--cloud-url", "https://api.example.test",
	})
	require.Error(t, err)

	assert.False(t, serviceCalled)
	assert.NoFileExists(t, filepath.Join(home, ".paxd", "secrets", "remotes", "default", "node_key"))
}

func stubRemoteLogin(t *testing.T, fn remoteLoginFunc) func() {
	t.Helper()
	previous := runRemoteLogin
	runRemoteLogin = fn
	return func() {
		runRemoteLogin = previous
	}
}

func stubServiceOps(t *testing.T, install func(serviceInstallOptions) error, control func(string, bool) error) func() {
	t.Helper()
	previousInstall := installPaxdService
	previousControl := controlPaxdService
	installPaxdService = install
	controlPaxdService = control
	return func() {
		installPaxdService = previousInstall
		controlPaxdService = previousControl
	}
}

func stubVerifyLocalAPI(t *testing.T, verify func(context.Context, time.Duration) error) func() {
	t.Helper()
	previous := verifyPaxdLocalAPI
	verifyPaxdLocalAPI = verify
	return func() {
		verifyPaxdLocalAPI = previous
	}
}

func flagNames(flags []cli.Flag) []string {
	names := make([]string, 0, len(flags))
	for _, flag := range flags {
		names = append(names, flag.Names()...)
	}
	return names
}

func openTestDaemonStore(t *testing.T, path string) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	t.Cleanup(func() { closeDaemonStore(store) })
	return store
}

func seedConversationRemote(t *testing.T, home string, cloudURL string, agentID string) {
	t.Helper()
	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	secretRef, err := (remotesecrets.Store{}).StoreNodeKey(context.Background(), "prod", "node-secret")
	require.NoError(t, err)
	enabled := true
	_, err = store.CreateRemote(context.Background(), control.CreateRemoteCommand{
		Remote: control.Remote{
			ID:          "prod",
			Name:        "Production",
			CloudAPIURL: cloudURL,
			NodeID:      "node_1",
			Enabled:     &enabled,
		},
		CloudAPIKeyRef: secretRef,
	})
	require.NoError(t, err)
	_, err = store.CreateAgentConnection(context.Background(), control.CreateAgentConnectionCommand{
		ID:           "conn_codex",
		RemoteID:     "prod",
		Name:         "codex",
		CloudAgentID: agentID,
		InstanceID:   "inst_1",
		AgentType:    "codex",
		Harness:      "codex",
		Command:      []string{"codex"},
		Enabled:      &enabled,
		DesiredState: control.DesiredStateRunning,
	})
	require.NoError(t, err)
}

func configWithDB(path string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Daemon.DBPath = path
	return &cfg
}
