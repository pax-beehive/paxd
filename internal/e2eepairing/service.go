package e2eepairing

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/e2ee"
)

const maxManagerPairingResponse = 64 * 1024

type Store interface {
	ListAgentConnections(
		ctx context.Context,
		filter control.ListAgentConnectionsQuery,
	) ([]control.AgentConnectionView, error)
	ListRemotes(ctx context.Context, filter control.ListRemotesQuery) ([]control.RemoteView, error)
}

type ServiceOptions struct {
	Store      Store
	Headers    auth.HeaderProvider
	RootKeys   e2ee.RootKeyProvider
	HTTPClient *http.Client
}

type Service struct {
	store    Store
	headers  auth.HeaderProvider
	rootKeys e2ee.RootKeyProvider
	http     *http.Client
}

type Result struct {
	PairingID string `json:"pairing_id"`
	AgentID   string `json:"agent_id"`
	DeviceID  string `json:"device_id"`
	KeyEpoch  int64  `json:"key_epoch"`
}

type managerPairingRequest struct {
	PairingID          string `json:"pairing_id"`
	NodeID             string `json:"node_id"`
	AgentID            string `json:"agent_id"`
	DeviceID           string `json:"device_id"`
	DeviceName         string `json:"device_name"`
	KeyEpoch           int64  `json:"key_epoch"`
	RecipientPublicKey string `json:"recipient_public_key"`
	SecretCommitment   string `json:"secret_commitment"`
}

type managerKeyPackage struct {
	SenderEphemeralPublicKey string `json:"sender_ephemeral_public_key"`
	Nonce                    string `json:"nonce"`
	Ciphertext               string `json:"ciphertext"`
}

type managerResponse[T any] struct {
	Data    T      `json:"data"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func New(opts ServiceOptions) *Service {
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{store: opts.Store, headers: opts.Headers, rootKeys: opts.RootKeys, http: client}
}

func (s *Service) CompletePairing(
	ctx context.Context,
	agentSelector string,
	pairingID string,
	encodedSecret string,
) (Result, error) {
	if s == nil || s.store == nil || s.headers == nil || s.rootKeys == nil {
		return Result{}, errors.New("E2EE pairing service is not configured")
	}
	connection, remote, err := s.resolveConnection(ctx, agentSelector)
	if err != nil {
		return Result{}, err
	}
	if connection.CloudAgentID == "" {
		return Result{}, errors.New("agent connection has no cloud agent id")
	}
	secret, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(encodedSecret))
	if err != nil || len(secret) < 16 {
		return Result{}, errors.New("pairing secret must be at least 16 bytes of base64")
	}
	headers, err := s.headers.Headers(ctx, connection.RemoteID)
	if err != nil {
		return Result{}, fmt.Errorf("load Manager authentication: %w", err)
	}
	managerURL := strings.TrimRight(remote.Remote.CloudAPIURL, "/") +
		"/api/v1/node/agents/" + url.PathEscape(connection.CloudAgentID) +
		"/e2ee/pairings/" + url.PathEscape(pairingID)
	request, err := getManagerData[managerPairingRequest](ctx, s.http, managerURL, headers)
	if err != nil {
		return Result{}, err
	}
	if request.PairingID != pairingID || request.AgentID != connection.CloudAgentID ||
		(remote.Remote.NodeID != "" && request.NodeID != remote.Remote.NodeID) {
		return Result{}, errors.New("Manager returned a mismatched E2EE pairing route")
	}
	recipientPublicKey, err := base64.StdEncoding.Strict().DecodeString(request.RecipientPublicKey)
	if err != nil || len(recipientPublicKey) != 65 {
		return Result{}, errors.New("Manager returned an invalid pairing public key")
	}
	commitment, err := base64.StdEncoding.Strict().DecodeString(request.SecretCommitment)
	if err != nil || len(commitment) != 32 {
		return Result{}, errors.New("Manager returned an invalid pairing secret commitment")
	}
	pairingContext := e2ee.PairingContext{
		PairingID: request.PairingID, AgentID: request.AgentID,
		DeviceID: request.DeviceID, KeyEpoch: request.KeyEpoch,
	}
	wantCommitment, err := e2ee.PairingSecretCommitment(
		secret, pairingContext, recipientPublicKey,
	)
	if err != nil {
		return Result{}, err
	}
	if subtle.ConstantTimeCompare(commitment, wantCommitment) != 1 {
		return Result{}, errors.New("pairing secret commitment does not match")
	}
	rootKey, err := s.rootKeys.RootKey(ctx, request.AgentID, request.KeyEpoch)
	if err != nil {
		return Result{}, fmt.Errorf("derive agent root key: %w", err)
	}
	wrapped, err := e2ee.WrapAgentRootKey(
		rootKey, recipientPublicKey, secret, pairingContext, nil,
	)
	if err != nil {
		return Result{}, err
	}
	keyPackage := managerKeyPackage{
		SenderEphemeralPublicKey: base64.StdEncoding.EncodeToString(
			wrapped.SenderEphemeralPublicKey,
		),
		Nonce:      base64.StdEncoding.EncodeToString(wrapped.Nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(wrapped.Ciphertext),
	}
	if _, err := postManagerData[managerKeyPackage, json.RawMessage](
		ctx, s.http, managerURL+"/package", headers, keyPackage,
	); err != nil {
		return Result{}, err
	}
	return Result{
		PairingID: request.PairingID, AgentID: request.AgentID,
		DeviceID: request.DeviceID, KeyEpoch: request.KeyEpoch,
	}, nil
}

func (s *Service) resolveConnection(
	ctx context.Context,
	agentSelector string,
) (control.AgentConnectionView, control.RemoteView, error) {
	connections, err := s.store.ListAgentConnections(
		ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true},
	)
	if err != nil {
		return control.AgentConnectionView{}, control.RemoteView{}, err
	}
	var connection control.AgentConnectionView
	for _, candidate := range connections {
		if candidate.ID == agentSelector || candidate.CloudAgentID == agentSelector {
			connection = candidate
			break
		}
	}
	if connection.ID == "" {
		return control.AgentConnectionView{}, control.RemoteView{}, errors.New("agent connection not found")
	}
	remotes, err := s.store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return control.AgentConnectionView{}, control.RemoteView{}, err
	}
	for _, remote := range remotes {
		if remote.Remote.ID == connection.RemoteID {
			return connection, remote, nil
		}
	}
	return control.AgentConnectionView{}, control.RemoteView{}, errors.New("remote not found")
}

func getManagerData[T any](
	ctx context.Context,
	client *http.Client,
	endpoint string,
	headers http.Header,
) (T, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		var zero T
		return zero, err
	}
	request.Header = headers.Clone()
	return doManagerRequest[T](client, request)
}

func postManagerData[Body any, Result any](
	ctx context.Context,
	client *http.Client,
	endpoint string,
	headers http.Header,
	body Body,
) (Result, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		var zero Result
		return zero, err
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, bytes.NewReader(raw),
	)
	if err != nil {
		var zero Result
		return zero, err
	}
	request.Header = headers.Clone()
	request.Header.Set("Content-Type", "application/json")
	return doManagerRequest[Result](client, request)
}

func doManagerRequest[T any](client *http.Client, request *http.Request) (T, error) {
	response, err := client.Do(request)
	if err != nil {
		var zero T
		return zero, err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxManagerPairingResponse))
	var envelope managerResponse[T]
	if err := decoder.Decode(&envelope); err != nil {
		var zero T
		return zero, fmt.Errorf("decode Manager E2EE pairing response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var zero T
		return zero, fmt.Errorf("Manager E2EE pairing request failed: status=%d message=%s", response.StatusCode, envelope.Message)
	}
	return envelope.Data, nil
}
