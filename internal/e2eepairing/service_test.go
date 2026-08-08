package e2eepairing

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPairingGivenLocalSecretWhenManagerReturnsCommittedRequestThenPublishesBrowserWrappedAgentRoot(t *testing.T) {
	t.Parallel()
	recipient, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	secret := []byte("0123456789abcdef")
	context := e2ee.PairingContext{
		PairingID: "pair_1", AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 3,
	}
	commitment, err := e2ee.PairingSecretCommitment(secret, context, recipient.PublicKey().Bytes())
	require.NoError(t, err)
	var published managerKeyPackage
	manager := fakeHTTPClient(func(r *http.Request) *http.Response {
		assert.Equal(t, "node-key", r.Header.Get("X-Pax-Key"))
		switch r.Method {
		case http.MethodGet:
			return managerHTTPResponse(t, managerPairingRequest{
				PairingID: context.PairingID, NodeID: "node_1", AgentID: context.AgentID,
				DeviceID: context.DeviceID, KeyEpoch: context.KeyEpoch,
				RecipientPublicKey: base64.StdEncoding.EncodeToString(recipient.PublicKey().Bytes()),
				SecretCommitment:   base64.StdEncoding.EncodeToString(commitment),
			}, http.StatusOK)
		case http.MethodPost:
			require.NoError(t, json.NewDecoder(r.Body).Decode(&published))
			return managerHTTPResponse(t, published, http.StatusCreated)
		default:
			return managerHTTPResponse(t, nil, http.StatusMethodNotAllowed)
		}
	})
	provider := e2ee.DerivedAgentRootKeyProvider{NodeSeed: make([]byte, 32)}
	service := New(ServiceOptions{
		Store: fakeStore{
			connections: []control.AgentConnectionView{{
				ID: "conn_1", RemoteID: "remote_1", CloudAgentID: context.AgentID,
			}},
			remotes: []control.RemoteView{{Remote: control.Remote{
				ID: "remote_1", CloudAPIURL: "https://manager.test", NodeID: "node_1",
			}}},
		},
		Headers:    fakeHeaders{header: http.Header{"X-Pax-Key": []string{"node-key"}}},
		RootKeys:   provider,
		HTTPClient: manager,
	})

	result, err := service.CompletePairing(
		contextBackground(), "conn_1", "pair_1", base64.StdEncoding.EncodeToString(secret),
	)
	require.NoError(t, err)
	assert.Equal(t, "device_1", result.DeviceID)

	root, err := provider.RootKey(contextBackground(), "agent_1", 3)
	require.NoError(t, err)
	opened, err := e2ee.UnwrapAgentRootKey(recipient.Bytes(), secret, context, e2ee.WrappedAgentRootKey{
		RecipientPublicKey:       recipient.PublicKey().Bytes(),
		SenderEphemeralPublicKey: mustDecodeBase64(t, published.SenderEphemeralPublicKey),
		Nonce:                    mustDecodeBase64(t, published.Nonce),
		Ciphertext:               mustDecodeBase64(t, published.Ciphertext),
	})
	require.NoError(t, err)
	assert.Equal(t, root, opened)
}

func TestPairingGivenWrongOutOfBandSecretWhenCommitmentCheckedThenPublishesNothing(t *testing.T) {
	t.Parallel()
	recipient, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	context := e2ee.PairingContext{
		PairingID: "pair_1", AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 1,
	}
	commitment, err := e2ee.PairingSecretCommitment(
		[]byte("correct-secret-1"), context, recipient.PublicKey().Bytes(),
	)
	require.NoError(t, err)
	posts := 0
	manager := fakeHTTPClient(func(r *http.Request) *http.Response {
		if r.Method == http.MethodPost {
			posts++
		}
		return managerHTTPResponse(t, managerPairingRequest{
			PairingID: context.PairingID, NodeID: "node_1", AgentID: context.AgentID,
			DeviceID: context.DeviceID, KeyEpoch: 1,
			RecipientPublicKey: base64.StdEncoding.EncodeToString(recipient.PublicKey().Bytes()),
			SecretCommitment:   base64.StdEncoding.EncodeToString(commitment),
		}, http.StatusOK)
	})
	service := New(ServiceOptions{
		Store: fakeStore{
			connections: []control.AgentConnectionView{{ID: "conn_1", RemoteID: "remote_1", CloudAgentID: "agent_1"}},
			remotes:     []control.RemoteView{{Remote: control.Remote{ID: "remote_1", CloudAPIURL: "https://manager.test"}}},
		},
		Headers:    fakeHeaders{header: http.Header{"X-Pax-Key": []string{"node-key"}}},
		RootKeys:   e2ee.DerivedAgentRootKeyProvider{NodeSeed: make([]byte, 32)},
		HTTPClient: manager,
	})

	_, err = service.CompletePairing(
		contextBackground(), "conn_1", "pair_1",
		base64.StdEncoding.EncodeToString([]byte("wrong-secret-123")),
	)

	require.ErrorContains(t, err, "secret commitment")
	assert.Zero(t, posts)
}

func TestPairingGivenInvalidLocalOrManagerStateWhenCompletedThenItFailsClosed(t *testing.T) {
	t.Parallel()
	secret := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, err := (&Service{}).CompletePairing(context.Background(), "agent_1", "pair_1", secret)
	assert.ErrorContains(t, err, "not configured")

	baseOptions := ServiceOptions{
		Store: fakeStore{
			connections: []control.AgentConnectionView{{ID: "conn_1", RemoteID: "remote_1", CloudAgentID: "agent_1"}},
			remotes:     []control.RemoteView{{Remote: control.Remote{ID: "remote_1", CloudAPIURL: "https://manager.test", NodeID: "node_1"}}},
		},
		Headers:  fakeHeaders{header: http.Header{"X-Pax-Key": []string{"node-key"}}},
		RootKeys: e2ee.DerivedAgentRootKeyProvider{NodeSeed: make([]byte, 32)},
	}
	tests := []struct {
		name     string
		mutate   func(*ServiceOptions)
		selector string
		secret   string
		contains string
	}{
		{name: "missing connection", mutate: func(o *ServiceOptions) { o.Store = fakeStore{} }, selector: "missing", secret: secret, contains: "connection not found"},
		{name: "missing remote", mutate: func(o *ServiceOptions) {
			o.Store = fakeStore{connections: []control.AgentConnectionView{{ID: "conn_1", RemoteID: "missing", CloudAgentID: "agent_1"}}}
		}, selector: "conn_1", secret: secret, contains: "remote not found"},
		{name: "missing cloud id", mutate: func(o *ServiceOptions) {
			o.Store = fakeStore{connections: []control.AgentConnectionView{{ID: "conn_1", RemoteID: "remote_1"}}, remotes: baseOptions.Store.(fakeStore).remotes}
		}, selector: "conn_1", secret: secret, contains: "cloud agent id"},
		{name: "short secret", selector: "conn_1", secret: base64.StdEncoding.EncodeToString([]byte("short")), contains: "at least 16 bytes"},
		{name: "header failure", mutate: func(o *ServiceOptions) { o.Headers = fakeHeaders{err: errors.New("locked")} }, selector: "conn_1", secret: secret, contains: "load Manager authentication"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opts := baseOptions
			if test.mutate != nil {
				test.mutate(&opts)
			}
			_, callErr := New(opts).CompletePairing(context.Background(), test.selector, "pair_1", test.secret)
			assert.ErrorContains(t, callErr, test.contains)
		})
	}
}

func TestPairingGivenUntrustedManagerPayloadWhenLoadedThenItIsRejectedBeforePublishing(t *testing.T) {
	t.Parallel()
	recipient, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	secretBytes := []byte("0123456789abcdef")
	pairingContext := e2ee.PairingContext{PairingID: "pair_1", AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 1}
	commitment, err := e2ee.PairingSecretCommitment(secretBytes, pairingContext, recipient.PublicKey().Bytes())
	require.NoError(t, err)
	valid := managerPairingRequest{
		PairingID: "pair_1", NodeID: "node_1", AgentID: "agent_1", DeviceID: "device_1", KeyEpoch: 1,
		RecipientPublicKey: base64.StdEncoding.EncodeToString(recipient.PublicKey().Bytes()),
		SecretCommitment:   base64.StdEncoding.EncodeToString(commitment),
	}
	tests := []struct {
		name     string
		mutate   func(*managerPairingRequest)
		contains string
	}{
		{name: "route mismatch", mutate: func(r *managerPairingRequest) { r.AgentID = "other" }, contains: "mismatched"},
		{name: "invalid public key", mutate: func(r *managerPairingRequest) { r.RecipientPublicKey = "bad" }, contains: "public key"},
		{name: "invalid commitment", mutate: func(r *managerPairingRequest) { r.SecretCommitment = "bad" }, contains: "commitment"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := valid
			test.mutate(&response)
			posts := 0
			service := pairingTestService(fakeHTTPClient(func(r *http.Request) *http.Response {
				if r.Method == http.MethodPost {
					posts++
				}
				return managerHTTPResponse(t, response, http.StatusOK)
			}))
			_, callErr := service.CompletePairing(context.Background(), "agent_1", "pair_1", base64.StdEncoding.EncodeToString(secretBytes))
			assert.ErrorContains(t, callErr, test.contains)
			assert.Zero(t, posts)
		})
	}
}

func TestPairingManagerClientGivenBadResponseWhenCalledThenReturnsUsefulError(t *testing.T) {
	t.Parallel()
	request, err := http.NewRequest(http.MethodGet, "https://manager.test/pair", nil)
	require.NoError(t, err)
	_, err = doManagerRequest[managerPairingRequest](fakeHTTPClient(func(*http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewBufferString("not-json"))}
	}), request)
	assert.ErrorContains(t, err, "decode Manager")

	_, err = doManagerRequest[managerPairingRequest](fakeHTTPClient(func(*http.Request) *http.Response {
		return managerHTTPResponse(t, nil, http.StatusForbidden)
	}), request)
	assert.ErrorContains(t, err, "status=403")
}

func pairingTestService(client *http.Client) *Service {
	return New(ServiceOptions{
		Store: fakeStore{
			connections: []control.AgentConnectionView{{ID: "conn_1", RemoteID: "remote_1", CloudAgentID: "agent_1"}},
			remotes:     []control.RemoteView{{Remote: control.Remote{ID: "remote_1", CloudAPIURL: "https://manager.test", NodeID: "node_1"}}},
		},
		Headers:    fakeHeaders{header: http.Header{"X-Pax-Key": []string{"node-key"}}},
		RootKeys:   e2ee.DerivedAgentRootKeyProvider{NodeSeed: make([]byte, 32)},
		HTTPClient: client,
	})
}

type fakeStore struct {
	connections []control.AgentConnectionView
	remotes     []control.RemoteView
}

func (s fakeStore) ListAgentConnections(context.Context, control.ListAgentConnectionsQuery) ([]control.AgentConnectionView, error) {
	return s.connections, nil
}

func (s fakeStore) ListRemotes(context.Context, control.ListRemotesQuery) ([]control.RemoteView, error) {
	return s.remotes, nil
}

type fakeHeaders struct {
	header http.Header
	err    error
}

func (h fakeHeaders) Headers(context.Context, string) (http.Header, error) {
	return h.header.Clone(), h.err
}

func managerHTTPResponse(t *testing.T, data any, status int) *http.Response {
	t.Helper()
	var body bytes.Buffer
	require.NoError(t, json.NewEncoder(&body).Encode(map[string]any{
		"data": data, "code": status, "message": "ok",
	}))
	return &http.Response{
		StatusCode: status, Header: make(http.Header),
		Body: io.NopCloser(bytes.NewReader(body.Bytes())),
	}
}

type roundTripFunc func(*http.Request) *http.Response

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request), nil
}

func fakeHTTPClient(handler roundTripFunc) *http.Client {
	return &http.Client{Transport: handler}
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	require.NoError(t, err)
	return decoded
}

func contextBackground() context.Context { return context.Background() }
