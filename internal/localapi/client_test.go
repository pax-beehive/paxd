package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientListsLocalSessionsWithEscapedQuery(t *testing.T) {
	transport := &fakeRoundTripper{body: `{"type":"local_sessions.list","local_sessions":{"items":[]}}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	result, err := client.ListLocalSessions(context.Background(), control.ListLocalSessionsQuery{Agent: "claude code", Limit: 10})

	require.NoError(t, err)
	assert.Equal(t, "/v1/local/sessions?agent=claude+code&limit=10", transport.request.URL.RequestURI())
	assert.Equal(t, control.QueryLocalSessionsList, result.Type)
}

func TestClientPostsHarnessDiscover(t *testing.T) {
	transport := &fakeRoundTripper{body: `{"type":"harnesses.discover","harnesses":{"items":[{"harness":"codex","display_name":"Codex","state":"available"}]}}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	result, err := client.DiscoverHarnesses(context.Background(), control.DiscoverHarnessesQuery{Probe: true})

	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, transport.request.Method)
	assert.Equal(t, "/v1/harnesses/discover", transport.request.URL.Path)
	require.NotNil(t, result.Harnesses)
	assert.Len(t, result.Harnesses.Items, 1)
}

func TestClientReturnsErrorForLocalAPIError(t *testing.T) {
	transport := &fakeRoundTripper{status: http.StatusInternalServerError, body: `{"type":"status.get","error":{"code":"internal_error","message":"boom"}}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	result, err := client.GetStatus(context.Background())

	require.Error(t, err)
	require.NotNil(t, result.Error)
	assert.Equal(t, control.ErrCodeInternal, result.Error.Code)
}

func TestClientRestartsAgentConnectionWithCommandID(t *testing.T) {
	transport := &fakeRoundTripper{body: `{"command_id":"cmd_1","ok":true,"status":"received"}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	ack, err := client.RestartAgentConnection(context.Background(), "cmd_1", "conn/1")

	require.NoError(t, err)
	assert.Equal(t, "cmd_1", ack.CommandID)
	assert.Equal(t, http.MethodPost, transport.request.Method)
	assert.Equal(t, "/v1/agent-connections/conn%2F1/restart", transport.request.URL.EscapedPath())
	assert.Equal(t, "cmd_1", transport.request.Header.Get(commandIDHeader))
}

func TestClientUpdatesRemoteWithEscapedPathAndCommandID(t *testing.T) {
	transport := &fakeRoundTripper{body: `{"command_id":"cmd_2","ok":true,"status":"received"}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}
	name := "Prod"

	ack, err := client.UpdateRemote(context.Background(), "cmd_2", "remote/prod", control.UpdateRemoteCommand{
		Remote: control.RemotePatch{Name: &name},
	})

	require.NoError(t, err)
	assert.Equal(t, "cmd_2", ack.CommandID)
	assert.Equal(t, http.MethodPatch, transport.request.Method)
	assert.Equal(t, "/v1/remotes/remote%2Fprod", transport.request.URL.EscapedPath())
	assert.Equal(t, "cmd_2", transport.request.Header.Get(commandIDHeader))
	var body control.UpdateRemoteCommand
	require.NoError(t, json.NewDecoder(transport.request.Body).Decode(&body))
	assert.Equal(t, "remote/prod", body.RemoteID)
	require.NotNil(t, body.Remote.Name)
	assert.Equal(t, "Prod", *body.Remote.Name)
}

func TestClientDeletesRemoteWithCascadeQuery(t *testing.T) {
	transport := &fakeRoundTripper{body: `{"command_id":"cmd_3","ok":true,"status":"received"}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	ack, err := client.DeleteRemote(context.Background(), "cmd_3", "remote/prod", true)

	require.NoError(t, err)
	assert.Equal(t, "cmd_3", ack.CommandID)
	assert.Equal(t, http.MethodDelete, transport.request.Method)
	assert.Equal(t, "/v1/remotes/remote%2Fprod", transport.request.URL.EscapedPath())
	assert.Equal(t, "true", transport.request.URL.Query().Get("cascade_agent_connections"))
	assert.Equal(t, "cmd_3", transport.request.Header.Get(commandIDHeader))
}

type fakeRoundTripper struct {
	request *http.Request
	status  int
	body    string
}

func (t *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.request = req
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewBufferString(t.body)),
		Request:    req,
	}, nil
}
