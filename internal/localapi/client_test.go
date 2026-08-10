package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/e2eepairing"
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

func TestClientUpdatesAgentConnectionWithEscapedPathAndCommandID(t *testing.T) {
	transport := &fakeRoundTripper{body: `{"command_id":"cmd_agent_stop","ok":true,"status":"received"}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}
	state := control.DesiredStateStopped

	ack, err := client.UpdateAgentConnection(context.Background(), "cmd_agent_stop", "conn/1", control.UpdateAgentConnectionCommand{
		DesiredState: &state,
	})

	require.NoError(t, err)
	assert.Equal(t, "cmd_agent_stop", ack.CommandID)
	assert.Equal(t, http.MethodPatch, transport.request.Method)
	assert.Equal(t, "/v1/agent-connections/conn%2F1", transport.request.URL.EscapedPath())
	assert.Equal(t, "cmd_agent_stop", transport.request.Header.Get(commandIDHeader))
	var body control.UpdateAgentConnectionCommand
	require.NoError(t, json.NewDecoder(transport.request.Body).Decode(&body))
	assert.Equal(t, "conn/1", body.ConnectionID)
	require.NotNil(t, body.DesiredState)
	assert.Equal(t, control.DesiredStateStopped, *body.DesiredState)
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

func TestClientCompletesE2EEPairingWithEscapedPathAndOpaqueSecret(t *testing.T) {
	transport := &fakeRoundTripper{body: `{"pairing_id":"pair/1","agent_id":"agent_1","device_id":"device_1","key_epoch":1}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	result, err := client.CompleteE2EEPairing(context.Background(), "agent_1", "pair/1", "opaque-secret")

	require.NoError(t, err)
	assert.Equal(t, e2eepairing.Result{PairingID: "pair/1", AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 1}, result)
	assert.Equal(t, "/v1/e2ee/pairings/pair%2F1/complete", transport.request.URL.EscapedPath())
	var body completeE2EEPairingRequest
	require.NoError(t, json.NewDecoder(transport.request.Body).Decode(&body))
	assert.Equal(t, "opaque-secret", body.PairingSecret)
}

func TestClientCompletesE2EEPairingGivenLocalAPIErrorThenReturnsError(t *testing.T) {
	transport := &fakeRoundTripper{status: http.StatusBadRequest, body: `{}`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	_, err := client.CompleteE2EEPairing(context.Background(), "agent_1", "pair_1", "secret")

	assert.ErrorContains(t, err, "Bad Request")
}

func TestClientCompletesE2EEPairingGivenMalformedSuccessThenReturnsDecodeError(t *testing.T) {
	transport := &fakeRoundTripper{body: `not-json`}
	client := &Client{baseURL: "http://paxd", http: &http.Client{Transport: transport}}

	_, err := client.CompleteE2EEPairing(context.Background(), "agent_1", "pair_1", "secret")

	assert.Error(t, err)
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
