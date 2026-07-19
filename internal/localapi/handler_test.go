package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/testkit/controltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDocsPageAndOpenAPIJSONAreServedWithoutControlService(t *testing.T) {
	handler := NewHandler(nil)

	docs := httptest.NewRecorder()
	handler.ServeHTTP(docs, httptest.NewRequest(http.MethodGet, "/docs", nil))

	require.Equal(t, http.StatusOK, docs.Code)
	assert.Contains(t, docs.Header().Get("Content-Type"), "text/html")
	assert.Contains(t, docs.Body.String(), "paxd debug API")
	assert.Contains(t, docs.Body.String(), "/openapi.json")
	assert.Contains(t, docs.Body.String(), "Request JSON")

	specResponse := httptest.NewRecorder()
	handler.ServeHTTP(specResponse, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))

	require.Equal(t, http.StatusOK, specResponse.Code)
	assert.Contains(t, specResponse.Header().Get("Content-Type"), "application/json")

	var spec map[string]any
	require.NoError(t, json.Unmarshal(specResponse.Body.Bytes(), &spec))
	assert.Equal(t, "3.0.3", spec["openapi"])
	assert.Contains(t, spec["paths"], "/v1/status")

	paths := spec["paths"].(map[string]any)
	remotes := paths["/v1/remotes"].(map[string]any)
	postRemote := remotes["post"].(map[string]any)
	requestBody := postRemote["requestBody"].(map[string]any)
	content := requestBody["content"].(map[string]any)
	jsonMedia := content["application/json"].(map[string]any)
	example := jsonMedia["example"].(map[string]any)
	assert.Contains(t, example, "remote")
}

func TestRootServesDocsPage(t *testing.T) {
	rec := httptest.NewRecorder()

	NewHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Endpoints")
}

func TestDocumentedEndpointsAreRoutable(t *testing.T) {
	handler := NewHandler(docsRouteService{})

	for _, endpoint := range localAPIEndpoints() {
		t.Run(endpoint.Method+" "+endpoint.Path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(endpoint.Method, docsRouteRequestPath(endpoint), strings.NewReader(docsRouteBody(endpoint)))
			req.Header.Set(commandIDHeader, "cmd_docs_route")
			if endpoint.RequestBody != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			handler.ServeHTTP(rec, req)

			assert.NotEqual(t, http.StatusNotFound, rec.Code, rec.Body.String())
			assert.NotEqual(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
		})
	}
}

func TestOpenAPIPathsMatchEndpointRegistry(t *testing.T) {
	spec := openAPISpec()
	paths, ok := spec["paths"].(map[string]any)
	require.True(t, ok)

	for _, endpoint := range localAPIEndpoints() {
		operations, ok := paths[endpoint.Path].(map[string]any)
		require.True(t, ok, endpoint.Path)
		assert.Contains(t, operations, methodKey(endpoint.Method))
	}
}

func TestGetRemotesMapsToControlQuery(t *testing.T) {
	service := controltest.NewMockService(t)
	query := controltest.LoadQueryRequest(t, "remotes_list")
	result := controltest.LoadQueryResponse(t, "remotes_list")
	service.ExpectQueryFrom(control.Source{Kind: control.SourceLocal}, query).ReturnQueryResult(result)

	req := httptest.NewRequest(http.MethodGet, "/v1/remotes?include_disabled=true", nil)
	rec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got control.QueryResult
	decodeResponse(t, rec, &got)
	if !equalJSON(t, got, result) {
		t.Fatalf("query result = %+v, want %+v", got, result)
	}
}

func TestPostRemoteCreateMapsToControlCommand(t *testing.T) {
	service := controltest.NewMockService(t)
	cmd := controltest.LoadCommandRequest(t, "remote_create")
	ack := controltest.LoadCommandResponse(t, "remote_create")
	service.ExpectCommandFrom(control.Source{Kind: control.SourceLocal}, cmd).ReturnCommandAck(ack)

	body := mustJSON(t, cmd.CreateRemote)
	req := httptest.NewRequest(http.MethodPost, "/v1/remotes", bytes.NewReader(body))
	req.Header.Set(commandIDHeader, cmd.CommandID)
	rec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got control.CommandAck
	decodeResponse(t, rec, &got)
	if !equalJSON(t, got, ack) {
		t.Fatalf("ack = %+v, want %+v", got, ack)
	}
}

func TestPatchAgentConnectionPreservesNilAndFalse(t *testing.T) {
	service := controltest.NewMockService(t)
	enabled := false
	cmd := control.Command{
		CommandID: "cmd_conn_update_enabled",
		Type:      control.CommandAgentConnectionUpdate,
		UpdateAgentConnection: &control.UpdateAgentConnectionCommand{
			ConnectionID: "conn_codex",
			Enabled:      &enabled,
		},
	}
	ack := control.CommandAck{CommandID: cmd.CommandID, OK: true, Status: control.CommandStatusReceived, TargetType: "agent_connection", TargetID: "conn_codex"}
	service.ExpectCommandFrom(control.Source{Kind: control.SourceLocal}, cmd).ReturnCommandAck(ack)

	req := httptest.NewRequest(http.MethodPatch, "/v1/agent-connections/conn_codex", strings.NewReader(`{"enabled":false}`))
	req.Header.Set(commandIDHeader, cmd.CommandID)
	rec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHarnessDiscoverAndLocalSessionSyncMapToQueries(t *testing.T) {
	service := controltest.NewMockService(t)
	discover := control.Query{
		Type:              control.QueryHarnessesDiscover,
		DiscoverHarnesses: &control.DiscoverHarnessesQuery{Probe: true, Names: []string{"codex"}},
	}
	service.ExpectQueryFrom(control.Source{Kind: control.SourceLocal}, discover).ReturnQueryResult(control.QueryResult{Type: control.QueryHarnessesDiscover, Harnesses: &control.ListHarnessesResult{}})

	sync := control.Query{
		Type:              control.QueryLocalSessionsSync,
		SyncLocalSessions: &control.SyncLocalSessionsQuery{Agent: "codex", Limit: 10},
	}
	service.ExpectQueryFrom(control.Source{Kind: control.SourceLocal}, sync).ReturnQueryResult(control.QueryResult{Type: control.QueryLocalSessionsSync, LocalSessionSync: &control.LocalSessionSyncResult{Synced: 2}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/harnesses/discover", strings.NewReader(`{"probe":true,"names":["codex"]}`))
	NewHandler(service).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("discover status = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/local/sessions/sync", strings.NewReader(`{"agent":"codex","limit":10}`))
	NewHandler(service).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestQueryRoutes(t *testing.T) {
	service := controltest.NewMockService(t)
	routes := []struct {
		name  string
		path  string
		query control.Query
	}{
		{
			name:  "status",
			path:  "/v1/status",
			query: control.Query{Type: control.QueryStatusGet, GetStatus: &control.GetStatusQuery{}},
		},
		{
			name:  "get remote",
			path:  "/v1/remotes/remote_prod",
			query: control.Query{Type: control.QueryRemoteGet, GetRemote: &control.GetRemoteQuery{RemoteID: "remote_prod"}},
		},
		{
			name: "list agent connections",
			path: "/v1/agent-connections?remote_id=remote_prod&include_disabled=true",
			query: control.Query{
				Type:                 control.QueryAgentConnectionsList,
				ListAgentConnections: &control.ListAgentConnectionsQuery{RemoteID: "remote_prod", IncludeDisabled: true},
			},
		},
		{
			name:  "get agent connection",
			path:  "/v1/agent-connections/conn_codex",
			query: control.Query{Type: control.QueryAgentConnectionGet, GetAgentConnection: &control.GetAgentConnectionQuery{ConnectionID: "conn_codex"}},
		},
		{
			name:  "list harnesses",
			path:  "/v1/harnesses?include_missing=true",
			query: control.Query{Type: control.QueryHarnessesList, ListHarnesses: &control.ListHarnessesQuery{IncludeMissing: true}},
		},
		{
			name:  "local overview",
			path:  "/v1/local/overview",
			query: control.Query{Type: control.QueryLocalOverviewGet, GetLocalOverview: &control.GetLocalOverviewQuery{}},
		},
		{
			name:  "list local sessions",
			path:  "/v1/local/sessions?agent=codex&limit=2",
			query: control.Query{Type: control.QueryLocalSessionsList, ListLocalSessions: &control.ListLocalSessionsQuery{Agent: "codex", Limit: 2}},
		},
		{
			name:  "get local session",
			path:  "/v1/local/sessions/codex:sess_1",
			query: control.Query{Type: control.QueryLocalSessionGet, GetLocalSession: &control.GetLocalSessionQuery{SessionID: "codex:sess_1"}},
		},
		{
			name:  "get command",
			path:  "/v1/commands/cmd_1",
			query: control.Query{Type: control.QueryCommandGet, GetCommand: &control.GetCommandQuery{CommandID: "cmd_1"}},
		},
	}

	for _, route := range routes {
		service.ExpectQueryFrom(control.Source{Kind: control.SourceLocal}, route.query).ReturnQueryResult(control.QueryResult{Type: route.query.Type})
	}
	handler := NewHandler(service)
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, route.path, nil)
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestCommandRoutes(t *testing.T) {
	service := controltest.NewMockService(t)
	remoteURL := "https://api2.example.test"
	auth := control.Command{
		CommandID: "cmd_remote_auth",
		Type:      control.CommandRemoteAuthConfigure,
		ConfigureRemoteAuth: &control.ConfigureRemoteAuthCommand{
			RemoteID: "remote_prod",
			Kind:     control.RemoteAuthCloudflareAccess,
			CloudflareAccess: &control.CloudflareAccessAuth{
				ClientID:        "cf-client",
				ClientSecretRef: "env:PAX_CF_SECRET",
			},
		},
	}
	createAgent := controltest.LoadCommandRequest(t, "agent_connection_create")
	routes := []struct {
		name   string
		method string
		path   string
		body   string
		cmd    control.Command
	}{
		{
			name:   "update remote",
			method: http.MethodPatch,
			path:   "/v1/remotes/remote_prod",
			body:   `{"remote":{"cloud_api_url":"https://api2.example.test"}}`,
			cmd: control.Command{
				CommandID: "cmd_remote_update",
				Type:      control.CommandRemoteUpdate,
				UpdateRemote: &control.UpdateRemoteCommand{
					RemoteID: "remote_prod",
					Remote:   control.RemotePatch{CloudAPIURL: &remoteURL},
				},
			},
		},
		{
			name:   "delete remote",
			method: http.MethodDelete,
			path:   "/v1/remotes/remote_prod?cascade_agent_connections=true",
			cmd: control.Command{
				CommandID:    "cmd_remote_delete",
				Type:         control.CommandRemoteDelete,
				DeleteRemote: &control.DeleteRemoteCommand{RemoteID: "remote_prod", CascadeAgentConnections: true},
			},
		},
		{
			name:   "restart remote",
			method: http.MethodPost,
			path:   "/v1/remotes/remote_prod/restart",
			cmd: control.Command{
				CommandID:     "cmd_remote_restart",
				Type:          control.CommandRemoteRestart,
				RestartRemote: &control.RestartRemoteCommand{RemoteID: "remote_prod"},
			},
		},
		{
			name:   "configure remote auth",
			method: http.MethodPut,
			path:   "/v1/remotes/remote_prod/auth",
			body:   `{"kind":"cloudflare_access","cloudflare_access":{"client_id":"cf-client","client_secret_ref":"env:PAX_CF_SECRET"}}`,
			cmd:    auth,
		},
		{
			name:   "clear remote auth",
			method: http.MethodDelete,
			path:   "/v1/remotes/remote_prod/auth",
			cmd: control.Command{
				CommandID:       "cmd_remote_auth_clear",
				Type:            control.CommandRemoteAuthClear,
				ClearRemoteAuth: &control.ClearRemoteAuthCommand{RemoteID: "remote_prod"},
			},
		},
		{
			name:   "create agent connection",
			method: http.MethodPost,
			path:   "/v1/agent-connections",
			body:   string(mustJSON(t, createAgent.CreateAgentConnection)),
			cmd:    createAgent,
		},
		{
			name:   "delete agent connection",
			method: http.MethodDelete,
			path:   "/v1/agent-connections/conn_codex?deregister=true",
			cmd: control.Command{
				CommandID:             "cmd_conn_delete",
				Type:                  control.CommandAgentConnectionDelete,
				DeleteAgentConnection: &control.DeleteAgentConnectionCommand{ConnectionID: "conn_codex", Deregister: true},
			},
		},
		{
			name:   "restart agent connection",
			method: http.MethodPost,
			path:   "/v1/agent-connections/conn_codex/restart",
			cmd: control.Command{
				CommandID:              "cmd_conn_restart",
				Type:                   control.CommandAgentConnectionRestart,
				RestartAgentConnection: &control.RestartAgentConnectionCommand{ConnectionID: "conn_codex"},
			},
		},
	}

	for _, route := range routes {
		ack := control.CommandAck{CommandID: route.cmd.CommandID, OK: true, Status: control.CommandStatusReceived}
		service.ExpectCommandFrom(control.Source{Kind: control.SourceLocal}, route.cmd).ReturnCommandAck(ack)
	}
	handler := NewHandler(service)
	for _, route := range routes {
		t.Run(route.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
			req.Header.Set(commandIDHeader, route.cmd.CommandID)
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestMalformedJSONReturnsBadRequestWithoutCallingService(t *testing.T) {
	service := controltest.NewMockService(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/remotes", strings.NewReader(`{"remote":`))
	rec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got control.ControlError
	decodeResponse(t, rec, &got)
	if got.Code != control.ErrCodeInvalidArgument {
		t.Fatalf("error = %+v", got)
	}
}

func TestRejectedAckAndServiceErrorStatusMapping(t *testing.T) {
	t.Run("rejected conflict", func(t *testing.T) {
		service := controltest.NewMockService(t)
		cmd := controltest.LoadCommandRequest(t, "remote_create")
		ack := control.CommandAck{
			CommandID: cmd.CommandID,
			OK:        false,
			Status:    control.CommandStatusRejected,
			Error:     &control.ControlError{Code: control.ErrCodeConflict, Message: "duplicate remote"},
		}
		service.ExpectCommandFrom(control.Source{Kind: control.SourceLocal}, cmd).ReturnCommandAck(ack)

		req := httptest.NewRequest(http.MethodPost, "/v1/remotes", bytes.NewReader(mustJSON(t, cmd.CreateRemote)))
		req.Header.Set(commandIDHeader, cmd.CommandID)
		rec := httptest.NewRecorder()
		NewHandler(service).ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("service error", func(t *testing.T) {
		service := controltest.NewMockService(t)
		query := control.Query{Type: control.QueryStatusGet, GetStatus: &control.GetStatusQuery{}}
		service.ExpectQueryFrom(control.Source{Kind: control.SourceLocal}, query).ReturnError(controltest.ErrMockService)

		req := httptest.NewRequest(http.MethodGet, "/v1/status", nil)
		rec := httptest.NewRecorder()
		NewHandler(service).ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})
}

func TestQueryResultErrorMapsByControlCode(t *testing.T) {
	service := controltest.NewMockService(t)
	query := control.Query{Type: control.QueryRemoteGet, GetRemote: &control.GetRemoteQuery{RemoteID: "missing"}}
	service.ExpectQueryFrom(control.Source{Kind: control.SourceLocal}, query).ReturnQueryResult(control.QueryResult{
		Type:  control.QueryRemoteGet,
		Error: &control.ControlError{Code: control.ErrCodeNotFound, Message: "remote not found"},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/remotes/missing", nil)
	rec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUnknownRouteReturnsNotFoundWithoutCallingService(t *testing.T) {
	service := controltest.NewMockService(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/nope", nil)
	rec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestGeneratedCommandIDWhenMissing(t *testing.T) {
	service := &captureService{}
	req := httptest.NewRequest(http.MethodPost, "/v1/remotes", strings.NewReader(`{"remote":{"id":"remote_prod","name":"Prod","cloud_api_url":"https://api.example.test"}}`))
	rec := httptest.NewRecorder()
	NewHandler(service).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if service.command.CommandID == "" {
		t.Fatal("generated command id is empty")
	}
	var ack control.CommandAck
	decodeResponse(t, rec, &ack)
	if ack.CommandID != service.command.CommandID {
		t.Fatalf("ack command id = %q, want %q", ack.CommandID, service.command.CommandID)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8765", true},
		{"localhost:8765", true},
		{"[::1]:8765", true},
		{"0.0.0.0:8765", false},
		{"192.168.1.10:8765", false},
	}
	for _, tc := range tests {
		if got := IsLoopbackAddr(tc.addr); got != tc.want {
			t.Fatalf("IsLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

type captureService struct {
	command control.Command
}

func (s *captureService) HandleCommand(ctx context.Context, src control.Source, cmd control.Command) (control.CommandAck, error) {
	_ = ctx
	_ = src
	s.command = cmd
	return control.CommandAck{CommandID: cmd.CommandID, OK: true, Status: control.CommandStatusReceived, TargetType: "remote", TargetID: cmd.CreateRemote.Remote.ID}, nil
}

func (s *captureService) HandleQuery(ctx context.Context, src control.Source, query control.Query) (control.QueryResult, error) {
	_ = ctx
	_ = src
	return control.QueryResult{Type: query.Type}, nil
}

type docsRouteService struct{}

func (s docsRouteService) HandleCommand(ctx context.Context, src control.Source, cmd control.Command) (control.CommandAck, error) {
	_ = ctx
	_ = src
	return control.CommandAck{CommandID: cmd.CommandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (s docsRouteService) HandleQuery(ctx context.Context, src control.Source, query control.Query) (control.QueryResult, error) {
	_ = ctx
	_ = src
	return control.QueryResult{Type: query.Type}, nil
}

func docsRouteRequestPath(endpoint apiEndpoint) string {
	path := strings.ReplaceAll(endpoint.Path, "{id}", "docs_id")
	switch endpoint.Path {
	case "/v1/remotes":
		if endpoint.Method == http.MethodGet {
			return path + "?include_disabled=true"
		}
	case "/v1/remotes/{id}":
		if endpoint.Method == http.MethodDelete {
			return path + "?cascade_agent_connections=true"
		}
	case "/v1/agent-connections":
		if endpoint.Method == http.MethodGet {
			return path + "?remote_id=default&include_disabled=true"
		}
	case "/v1/agent-connections/{id}":
		if endpoint.Method == http.MethodDelete {
			return path + "?deregister=true"
		}
	case "/v1/harnesses":
		return path + "?include_missing=true"
	case "/v1/local/sessions":
		return path + "?agent=codex&limit=1"
	}
	return path
}

func docsRouteBody(endpoint apiEndpoint) string {
	switch endpoint.RequestBody {
	case "CreateRemoteCommand":
		return `{"remote":{"id":"default","name":"Default","cloud_api_url":"https://api.example.test"}}`
	case "UpdateRemoteCommand":
		return `{"remote":{"enabled":true}}`
	case "ConfigureRemoteAuthCommand":
		return `{"kind":"none"}`
	case "CreateAgentConnectionCommand":
		return `{"remote_id":"default","name":"codex","instance_id":"default","agent_type":"codex","harness":"codex","command":["codex"]}`
	case "UpdateAgentConnectionCommand":
		return `{"enabled":true}`
	case "DiscoverHarnessesQuery":
		return `{"probe":true}`
	case "SyncLocalSessionsQuery":
		return `{"agent":"codex","limit":1}`
	default:
		return ""
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return raw
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder, dest any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dest); err != nil {
		t.Fatalf("response JSON decode error = %v body=%s", err, rec.Body.String())
	}
}

func equalJSON(t *testing.T, got, want any) bool {
	t.Helper()
	gotJSON := mustJSON(t, got)
	wantJSON := mustJSON(t, want)
	return bytes.Equal(gotJSON, wantJSON)
}

type diagnosticsQueryService struct {
	lastQuery control.Query
}

func (s *diagnosticsQueryService) HandleCommand(ctx context.Context, src control.Source, cmd control.Command) (control.CommandAck, error) {
	_ = ctx
	_ = src
	return control.CommandAck{CommandID: cmd.CommandID, OK: true, Status: control.CommandStatusReceived}, nil
}

func (s *diagnosticsQueryService) HandleQuery(ctx context.Context, src control.Source, query control.Query) (control.QueryResult, error) {
	_ = ctx
	_ = src
	s.lastQuery = query
	return control.QueryResult{
		Type:        query.Type,
		Diagnostics: &control.DiagnosticsView{RuntimeDiagnostics: control.RuntimeDiagnostics{PaxdVersion: "v-test"}},
	}, nil
}

func TestRouteDiagnosticsGet(t *testing.T) {
	service := &diagnosticsQueryService{}
	handler := NewHandler(service)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/diagnostics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var result control.QueryResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.NotNil(t, result.Diagnostics)
	assert.Equal(t, "v-test", result.Diagnostics.PaxdVersion)
	assert.Equal(t, control.QueryDiagnosticsGet, service.lastQuery.Type)
	assert.NotNil(t, service.lastQuery.GetDiagnostics)
}
