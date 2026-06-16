// Package cloud provides an HTTP client for the Fleet Cloud API.
package cloud

import (
	"bytes"
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
	SessionID     string `json:"session_id"`
	AgentType     string `json:"agent_type,omitempty"`
	NativeID      string `json:"native_id,omitempty"`
	Name          string `json:"name,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
	Preview       string `json:"preview,omitempty"`
	Status        string `json:"status,omitempty"`
	CurrentTask   string `json:"current_task,omitempty"`
	MessageCount  int    `json:"message_count,omitempty"`
	TokenUsage    int64  `json:"token_usage,omitempty"`
	Model         string `json:"model,omitempty"`
	RunID         string `json:"run_id,omitempty"`
	RunStatus     string `json:"run_status,omitempty"`
	LastMessageAt string `json:"last_message_at,omitempty"`
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
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
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

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register node: status %d: %s", resp.StatusCode, string(body))
	}

	return decodeEnvelope[RegisterNodeResponse](resp.Body, "register node")
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

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register node agent: status %d: %s", resp.StatusCode, string(body))
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
	var envelope APIResponse[T]
	if err := json.NewDecoder(body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", op, err)
	}
	return &envelope.Data, nil
}

func checkStatus(resp *http.Response, op string) error {
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: status %d: %s", op, resp.StatusCode, string(body))
	}
	return nil
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
