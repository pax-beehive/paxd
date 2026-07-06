// Package cloud provides an HTTP client for the Fleet Cloud API.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/pkg/model"
)

// Client communicates with the Fleet Cloud API.
type Client struct {
	baseURL        string
	apiKey         string
	cfClientID     string
	cfClientSecret string
	httpClient     *http.Client
}

// APIResponse is the standard pax-manager response envelope.
type APIResponse[T any] struct {
	Data    T      `json:"data"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NodeStatusReport is the payload sent to POST /api/v1/node/status.
type NodeStatusReport struct {
	NodeID   string        `json:"node_id,omitempty"`
	Hostname string        `json:"hostname,omitempty"`
	Agents   []AgentStatus `json:"agents"`
	System   SystemMetrics `json:"system"`
	Metadata any           `json:"metadata,omitempty"`
}

// AgentStatus describes one agent hosted by this node.
type AgentStatus struct {
	AgentID   string          `json:"agent_id"`
	Name      string          `json:"name,omitempty"`
	AgentType string          `json:"agent_type,omitempty"`
	Status    string          `json:"status,omitempty"`
	Online    bool            `json:"online"`
	Sessions  []SessionStatus `json:"sessions,omitempty"`
}

// SessionStatus describes one Hermes session.
type SessionStatus struct {
	SessionID      string     `json:"session_id"`
	AgentType      string     `json:"agent_type,omitempty"`
	NativeID       string     `json:"native_id,omitempty"`
	Name           string     `json:"name,omitempty"`
	ProjectID      string     `json:"project_id,omitempty"`
	Preview        string     `json:"preview,omitempty"`
	WorkspaceRoots []string   `json:"workspace_roots,omitempty"`
	Source         string     `json:"source,omitempty"`
	Status         string     `json:"status,omitempty"`
	CurrentTask    string     `json:"current_task,omitempty"`
	MessageCount   int        `json:"message_count,omitempty"`
	TokenUsage     TokenUsage `json:"token_usage,omitempty"`
	Model          string     `json:"model,omitempty"`
	RunID          string     `json:"run_id,omitempty"`
	RunStatus      string     `json:"run_status,omitempty"`
	LastMessageAt  string     `json:"last_message_at,omitempty"`
}

type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens,omitempty"`
	OutputTokens int64 `json:"output_tokens,omitempty"`
	TotalTokens  int64 `json:"total_tokens,omitempty"`
}

type AgentSessionsReport struct {
	Sessions []SessionStatus `json:"sessions"`
}

// SystemMetrics holds machine-level metrics.
type SystemMetrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	UptimeSeconds int64   `json:"uptime_seconds"`
}

// Message is a message from/to the Cloud inbox.
type Message struct {
	ID          int64           `json:"id"`
	MessageID   string          `json:"message_id"`
	NodeID      string          `json:"node_id,omitempty"`
	AgentID     string          `json:"agent_id"`
	SessionID   string          `json:"session_id,omitempty"`
	MessageType string          `json:"message_type"`
	Message     string          `json:"message"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Status      string          `json:"status,omitempty"`
	CreatedAt   string          `json:"created_at"`
	Type        string          `json:"-"`
	Content     string          `json:"-"`
}

// OutboundMessage is the response sent back to Cloud after message execution.
type OutboundMessage struct {
	AgentID     string `json:"agent_id"`
	SessionID   string `json:"session_id,omitempty"`
	MessageType string `json:"message_type,omitempty"`
	Content     string `json:"content"`
	ParentMsgID string `json:"parent_message_id,omitempty"`

	// Structured turn result fields (type="turn_result")
	TurnID      string              `json:"turn_id,omitempty"`
	ResponseID  string              `json:"response_id,omitempty"`
	Status      string              `json:"status,omitempty"` // "completed" | "cancelled" | "error"
	Events      []any               `json:"events,omitempty"` // model.* structs
	FileChanges []*model.FileChange `json:"file_changes,omitempty"`
	TokenUsage  *model.UsageInfo    `json:"token_usage,omitempty"`
}

// RegisterNodeRequest is the payload for POST /api/v1/node/register.
type RegisterNodeRequest struct {
	Name        string `json:"name,omitempty"`
	Hostname    string `json:"hostname"`
	MachineType string `json:"machine_type"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	PaxdVersion string `json:"paxd_version,omitempty"`
	APIEndpoint string `json:"api_endpoint,omitempty"`
}

// RegisterNodeResponse is the response from POST /api/v1/node/register.
type RegisterNodeResponse struct {
	NodeID string `json:"node_id"`
	APIKey string `json:"api_key"`
}

// StartNodeRegistrationRequest starts interactive node onboarding.
type StartNodeRegistrationRequest struct {
	Name        string `json:"name,omitempty"`
	Hostname    string `json:"hostname"`
	MachineType string `json:"machine_type,omitempty"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	PaxdVersion string `json:"paxd_version,omitempty"`
	APIEndpoint string `json:"api_endpoint,omitempty"`
}

// StartNodeRegistrationResponse contains the pairing code and poll credential.
type StartNodeRegistrationResponse struct {
	RegistrationID          string `json:"registration_id"`
	PairCode                string `json:"pair_code"`
	PollToken               string `json:"poll_token"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
	ExpiresAt               string `json:"expires_at"`
}

// PollNodeRegistrationRequest polls an interactive node onboarding session.
type PollNodeRegistrationRequest struct {
	RegistrationID string `json:"registration_id"`
	PollToken      string `json:"poll_token"`
}

// PollNodeRegistrationResponse returns current onboarding status.
type PollNodeRegistrationResponse struct {
	Status string `json:"status"`
	NodeID string `json:"node_id,omitempty"`
	APIKey string `json:"api_key,omitempty"`
}

// RegisterNodeAgentRequest is the payload for POST /api/v1/node/agents/register.
type RegisterNodeAgentRequest struct {
	Node  *RegisterNodeRequest     `json:"node,omitempty"`
	Agent RegisterNodeAgentPayload `json:"agent"`
}

// RegisterNodeAgentPayload describes the agent to create under a node.
type RegisterNodeAgentPayload struct {
	Name      string `json:"name,omitempty"`
	AgentType string `json:"agent_type,omitempty"`
}

// RegisterNodeAgentResponse is the response from POST /api/v1/node/agents/register.
type RegisterNodeAgentResponse struct {
	NodeID  string `json:"node_id"`
	APIKey  string `json:"api_key,omitempty"`
	AgentID string `json:"agent_id"`
}

// ResolveSecretRequest asks pax-manager to return a secret value for an agent.
type ResolveSecretRequest struct {
	SecretID  string `json:"secret_id"`
	Version   string `json:"version,omitempty"`
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id,omitempty"`
}

// ResolveSecretResponse is returned by POST /api/v1/node/secrets/resolve.
type ResolveSecretResponse struct {
	Status        string `json:"status"`
	ApprovalID    string `json:"approval_id,omitempty"`
	SecretID      string `json:"secret_id,omitempty"`
	VersionID     string `json:"version_id,omitempty"`
	VersionNumber int64  `json:"version_number,omitempty"`
	Value         string `json:"value,omitempty"`
}

// WriteSecretVersionRequest creates a new version in the cloud secret vault.
type WriteSecretVersionRequest struct {
	AgentID                  string `json:"agent_id"`
	SessionID                string `json:"session_id,omitempty"`
	Value                    string `json:"value"`
	MakeCurrent              bool   `json:"make_current"`
	ExpectedCurrentVersionID string `json:"expected_current_version_id,omitempty"`
	IdempotencyKey           string `json:"idempotency_key,omitempty"`
	Reason                   string `json:"reason,omitempty"`
}

// WriteSecretVersionResponse is returned by POST /api/v1/node/secrets/:id/versions.
type WriteSecretVersionResponse struct {
	Status        string `json:"status"`
	ApprovalID    string `json:"approval_id,omitempty"`
	SecretID      string `json:"secret_id,omitempty"`
	VersionID     string `json:"version_id,omitempty"`
	VersionNumber int64  `json:"version_number,omitempty"`
	Current       bool   `json:"current,omitempty"`
}

// ConversationDeliveryRequest is sent to POST /api/v1/node/conversation/deliver.
type ConversationDeliveryRequest struct {
	Source      *ConversationDeliverySource `json:"source,omitempty"`
	Target      ConversationDeliveryTarget  `json:"target"`
	Context     ConversationDeliveryContext `json:"context"`
	Instruction string                      `json:"instruction,omitempty"`
	Reason      string                      `json:"reason,omitempty"`
}

type ConversationDeliverySource struct {
	AgentID               string `json:"agent_id,omitempty"`
	RepresentativeAgentID string `json:"representative_agent_id,omitempty"`
	SessionID             string `json:"session_id,omitempty"`
}

type ConversationDeliveryTarget struct {
	Kind                  string `json:"kind"`
	RepresentativeAgentID string `json:"representative_agent_id,omitempty"`
	SessionID             string `json:"session_id,omitempty"`
	InvocationID          string `json:"invocation_id,omitempty"`
}

type ConversationDeliveryContext struct {
	LatestResponse bool `json:"latest_response"`
}

type ConversationDeliveryResponse struct {
	ContractVersion  string          `json:"contract_version,omitempty"`
	DeliveryEndpoint string          `json:"delivery_endpoint,omitempty"`
	ReceiptToken     string          `json:"receipt_token,omitempty"`
	Delivery         json.RawMessage `json:"delivery,omitempty"`
}

// UpgradeBinary is the response from GET /api/agent/upgrade.
type UpgradeBinary struct {
	Version string
	SHA256  string
	Body    io.ReadCloser
}

// NewClient creates a new Cloud API client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// WithCloudflareAccess attaches Cloudflare Access service-token headers.
func (c *Client) WithCloudflareAccess(clientID, clientSecret string) *Client {
	c.cfClientID = clientID
	c.cfClientSecret = clientSecret
	return c
}

func (c *Client) do(method, path string, body any) (*http.Response, error) {
	return c.doWithHeaders(method, path, body, nil)
}

func (c *Client) doWithHeaders(
	method string,
	path string,
	body any,
	headers map[string]string,
) (*http.Response, error) {
	return c.doWithContextAndHeaders(context.Background(), method, path, body, headers)
}

func (c *Client) doWithContextAndHeaders(
	ctx context.Context,
	method string,
	path string,
	body any,
	headers map[string]string,
) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-Pax-Key", c.apiKey)
	}
	if c.cfClientID != "" {
		req.Header.Set("CF-Access-Client-Id", c.cfClientID)
	}
	if c.cfClientSecret != "" {
		req.Header.Set("CF-Access-Client-Secret", c.cfClientSecret)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "paxd/0.1.0")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	return c.httpClient.Do(req)
}

// RegisterNode calls POST /api/v1/node/register.
func (c *Client) RegisterNode(
	req *RegisterNodeRequest,
	registrationToken string,
) (*RegisterNodeResponse, error) {
	resp, err := c.doWithHeaders(
		http.MethodPost,
		"/api/v1/node/register",
		req,
		map[string]string{"X-Registration-Token": registrationToken},
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkStatus(resp, "register node"); err != nil {
		return nil, err
	}

	return decodeEnvelope[RegisterNodeResponse](resp.Body, "register node")
}

// StartNodeRegistration begins interactive device-code style onboarding.
func (c *Client) StartNodeRegistration(
	req *StartNodeRegistrationRequest,
) (*StartNodeRegistrationResponse, error) {
	resp, err := c.do(http.MethodPost, "/api/v1/node/registration/start", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("start node registration: status %d: %s", resp.StatusCode, string(body))
	}
	return decodeEnvelope[StartNodeRegistrationResponse](resp.Body, "start node registration")
}

// PollNodeRegistration waits for the browser user to approve onboarding.
func (c *Client) PollNodeRegistration(
	req *PollNodeRegistrationRequest,
) (*PollNodeRegistrationResponse, error) {
	resp, err := c.do(http.MethodPost, "/api/v1/node/registration/poll", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("poll node registration: status %d: %s", resp.StatusCode, string(body))
	}
	return decodeEnvelope[PollNodeRegistrationResponse](resp.Body, "poll node registration")
}

// RegisterNodeAgent creates a cloud agent under the current node. With a
// registration token it also creates the node and returns the new node API key.
func (c *Client) RegisterNodeAgent(
	req *RegisterNodeAgentRequest,
	registrationToken string,
) (*RegisterNodeAgentResponse, error) {
	resp, err := c.doWithHeaders(
		http.MethodPost,
		"/api/v1/node/agents/register",
		req,
		map[string]string{"X-Registration-Token": registrationToken},
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkStatus(resp, "register node agent"); err != nil {
		return nil, err
	}

	return decodeEnvelope[RegisterNodeAgentResponse](resp.Body, "register node agent")
}

// PostNodeStatus sends a status report to POST /api/v1/node/status.
func (c *Client) PostNodeStatus(report *NodeStatusReport) error {
	resp, err := c.do(http.MethodPost, "/api/v1/node/status", report)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post node status: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *Client) PostAgentSessions(agentID string, report *AgentSessionsReport) error {
	return c.PostAgentSessionsContext(context.Background(), agentID, report)
}

func (c *Client) PostAgentSessionsContext(
	ctx context.Context,
	agentID string,
	report *AgentSessionsReport,
) error {
	path := "/api/v1/node/agents/" + url.PathEscape(agentID) + "/sessions"
	resp, err := c.doWithContextAndHeaders(ctx, http.MethodPost, path, report, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post agent sessions: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// FetchNodeMessages pulls unprocessed node-level messages.
func (c *Client) FetchNodeMessages(lastOffset int64, limit int) ([]Message, int64, bool, error) {
	return c.fetchMessages("/api/v1/node/mailbox", lastOffset, limit)
}

// FetchAgentMessages pulls unprocessed messages for an agent hosted by this node.
func (c *Client) FetchAgentMessages(
	agentID string,
	lastOffset int64,
	limit int,
) ([]Message, int64, bool, error) {
	path := "/api/v1/node/agents/" + url.PathEscape(agentID) + "/mailbox"
	return c.fetchMessages(path, lastOffset, limit)
}

// FetchAgentSessionMessages pulls unprocessed messages for a specific agent session.
func (c *Client) FetchAgentSessionMessages(
	agentID string,
	sessionID string,
	lastOffset int64,
	limit int,
) ([]Message, int64, bool, error) {
	path := "/api/v1/node/agents/" + url.PathEscape(agentID) +
		"/sessions/" + url.PathEscape(sessionID) + "/mailbox"
	return c.fetchMessages(path, lastOffset, limit)
}

func (c *Client) fetchMessages(
	path string,
	lastOffset int64,
	limit int,
) ([]Message, int64, bool, error) {
	if limit <= 0 {
		limit = 10
	}
	path = fmt.Sprintf("%s?offset=%d&limit=%d", path, lastOffset, limit)
	resp, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return nil, lastOffset, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, lastOffset, false, fmt.Errorf("fetch messages: status %d: %s", resp.StatusCode, string(body))
	}

	pull, err := decodeEnvelope[MailboxPull](resp.Body, "fetch messages")
	if err != nil {
		return nil, lastOffset, false, err
	}
	return pull.Messages, pull.MaxOffset, pull.HasMore, nil
}

// MailboxPull is the data payload returned by node mailbox endpoints.
type MailboxPull struct {
	Messages  []Message `json:"messages"`
	MaxOffset int64     `json:"max_offset"`
	HasMore   bool      `json:"has_more"`
}

// MarkDelivered confirms receipt of a message.
func (c *Client) MarkDelivered(messageID string) error {
	resp, err := c.do(
		http.MethodPost,
		"/api/v1/node/messages/"+url.PathEscape(messageID)+"/delivered",
		nil,
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp, "mark delivered")
}

// CreateOutbound creates an outbound message on the Cloud.
func (c *Client) CreateOutbound(msg *OutboundMessage) error {
	resp, err := c.do(http.MethodPost, "/api/v1/node/messages/outbound", msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return checkStatus(resp, "create outbound")
}

// ResolveSecret returns a plaintext secret value, or approval_required status.
func (c *Client) ResolveSecret(req *ResolveSecretRequest) (*ResolveSecretResponse, error) {
	resp, err := c.do(http.MethodPost, "/api/v1/node/secrets/resolve", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("resolve secret: status %d: %s", resp.StatusCode, string(body))
	}
	return decodeEnvelope[ResolveSecretResponse](resp.Body, "resolve secret")
}

// WriteSecretVersion appends a secret version, or returns approval_required status.
func (c *Client) WriteSecretVersion(
	secretID string,
	req *WriteSecretVersionRequest,
) (*WriteSecretVersionResponse, error) {
	resp, err := c.do(
		http.MethodPost,
		"/api/v1/node/secrets/"+url.PathEscape(secretID)+"/versions",
		req,
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("write secret version: status %d: %s", resp.StatusCode, string(body))
	}
	return decodeEnvelope[WriteSecretVersionResponse](resp.Body, "write secret version")
}

// PostConversationDelivery stores an agent-to-agent conversation delivery.
func (c *Client) PostConversationDelivery(req *ConversationDeliveryRequest) (*ConversationDeliveryResponse, error) {
	resp, err := c.do(http.MethodPost, "/api/v1/node/conversation/deliver", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("post conversation delivery: status %d: %s", resp.StatusCode, bodySnippet(body))
	}
	return decodeEnvelope[ConversationDeliveryResponse](resp.Body, "post conversation delivery")
}

// ReportCompleted marks a message as completed.
func (c *Client) ReportCompleted(messageID string, result *OutboundMessage) error {
	payload := map[string]any{
		"status":            "completed",
		"result_message_id": result.ResponseID,
		"content":           result.Content,
		"events":            result.Events,
		"file_changes":      result.FileChanges,
		"token_usage":       result.TokenUsage,
	}
	resp, err := c.do(
		http.MethodPost,
		"/api/v1/node/messages/"+url.PathEscape(messageID)+"/result",
		payload,
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp, "report completed")
}

// ReportFailure reports a failed message execution.
func (c *Client) ReportFailure(messageID string, errMsg string) error {
	payload := map[string]string{"status": "failed", "error": errMsg}
	resp, err := c.do(
		http.MethodPost,
		"/api/v1/node/messages/"+url.PathEscape(messageID)+"/result",
		payload,
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp, "report failure")
}

// UpdateOffset updates the node's message offset on the Cloud.
func (c *Client) UpdateOffset(offset int64) error {
	payload := map[string]int64{"offset": offset}
	resp, err := c.do(http.MethodPost, "/api/v1/node/messages/offset", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkStatus(resp, "update offset")
}

// FetchUpgrade downloads a new binary version.
func (c *Client) FetchUpgrade(version string) (*UpgradeBinary, error) {
	resp, err := c.do(http.MethodGet, "/api/agent/upgrade?version="+version, nil)
	if err != nil {
		return nil, err
	}
	// Do NOT close the body — caller must read and close it.

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("fetch upgrade: status %d: %s", resp.StatusCode, string(body))
	}

	sha := resp.Header.Get("X-Binary-SHA256")
	return &UpgradeBinary{
		Version: version,
		SHA256:  sha,
		Body:    resp.Body,
	}, nil
}

func decodeEnvelope[T any](body io.Reader, op string) (*T, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read %s response: %w", op, err)
	}
	var envelope APIResponse[T]
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf(
			"decode %s response: %w; body starts with %q",
			op,
			err,
			bodySnippet(data),
		)
	}
	return &envelope.Data, nil
}

func checkStatus(resp *http.Response, op string) error {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		detail := fmt.Sprintf(
			"%s: status %d content-type %q",
			op,
			resp.StatusCode,
			resp.Header.Get("Content-Type"),
		)
		if location := resp.Header.Get("Location"); location != "" {
			detail += fmt.Sprintf(" location %q", location)
		}
		if len(body) > 0 {
			detail += fmt.Sprintf(": %s", bodySnippet(body))
		}
		return fmt.Errorf("%s", detail)
	}
	return nil
}

func bodySnippet(body []byte) string {
	const maxSnippetBytes = 512
	body = bytes.TrimSpace(body)
	if len(body) > maxSnippetBytes {
		body = body[:maxSnippetBytes]
	}
	return string(body)
}

func (m *Message) UnmarshalJSON(data []byte) error {
	type wire Message
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Message(decoded)
	m.Type = firstNonEmpty(m.MessageType, m.Type)
	m.Content = firstNonEmpty(m.Message, m.Content)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
