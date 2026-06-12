// Package cloud provides an HTTP client for the Fleet Cloud API.
package cloud

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client communicates with the Fleet Cloud API.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// StatusReport is the payload sent to POST /api/agent/status.
type StatusReport struct {
	AgentID  string           `json:"agent_id"`
	Status   string           `json:"status"` // "online"
	Sessions []SessionStatus  `json:"sessions"`
	System   SystemMetrics    `json:"system"`
}

// SessionStatus describes one Hermes session.
type SessionStatus struct {
	SessionID    string `json:"session_id"`
	Status       string `json:"status"`        // "idle", "running", "completed"
	CurrentTask  string `json:"current_task"`
	TokenUsage   int64  `json:"token_usage"`
	LastActiveAt string `json:"last_active_at"`
}

// SystemMetrics holds machine-level metrics.
type SystemMetrics struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemoryPercent float64 `json:"memory_percent"`
	UptimeSeconds int64   `json:"uptime_seconds"`
}

// Message is a message from/to the Cloud inbox.
type Message struct {
	ID        int64  `json:"id"`
	MessageID string `json:"message_id"`
	AgentID   string `json:"agent_id"`
	SessionID string `json:"session_id,omitempty"`
	Type      string `json:"type"`      // "chat", "steer", "command"
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// OutboundMessage is the response sent back to Cloud after execution.
type OutboundMessage struct {
	AgentID     string `json:"agent_id"`
	SessionID   string `json:"session_id"`
	Type        string `json:"type"` // "chat_response", "error", "command_ack"
	Content     string `json:"content"`
	ParentMsgID string `json:"parent_message_id,omitempty"`
}

// RegisterRequest is the payload for POST /api/agent/register.
type RegisterRequest struct {
	Hostname    string `json:"hostname"`
	MachineType string `json:"machine_type"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
}

// RegisterResponse is the response from POST /api/agent/register.
type RegisterResponse struct {
	AgentID string `json:"agent_id"`
	APIKey  string `json:"api_key"`
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
		baseURL: baseURL,
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) do(method, path string, body any) (*http.Response, error) {
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
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "paxd/0.1.0")

	return c.httpClient.Do(req)
}

// Register calls POST /api/agent/register.
func (c *Client) Register(req *RegisterRequest) (*RegisterResponse, error) {
	resp, err := c.do(http.MethodPost, "/api/agent/register", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("register: status %d: %s", resp.StatusCode, string(body))
	}

	var regResp RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		return nil, fmt.Errorf("decode register response: %w", err)
	}
	return &regResp, nil
}

// PostStatus sends a status report to POST /api/agent/status.
func (c *Client) PostStatus(report *StatusReport) error {
	resp, err := c.do(http.MethodPost, "/api/agent/status", report)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post status: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// FetchMessages pulls unprocessed messages from GET /api/agent/messages.
func (c *Client) FetchMessages(lastOffset int64) ([]Message, error) {
	url := fmt.Sprintf("/api/agent/messages?offset=%d", lastOffset)
	resp, err := c.do(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("fetch messages: status %d: %s", resp.StatusCode, string(body))
	}

	var msgs []Message
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	return msgs, nil
}

// MarkDelivered confirms receipt of a message (POST /api/agent/messages/{id}/delivered).
func (c *Client) MarkDelivered(messageID string) error {
	resp, err := c.do(http.MethodPost, "/api/agent/messages/"+messageID+"/delivered", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// CreateOutbound creates an outbound message on the Cloud.
func (c *Client) CreateOutbound(msg *OutboundMessage) error {
	resp, err := c.do(http.MethodPost, "/api/agent/messages/outbound", msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create outbound: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ReportCompleted marks a message as completed (POST /api/agent/messages/{id}/completed).
func (c *Client) ReportCompleted(messageID string, resultMessageID string) error {
	payload := map[string]string{"result_message_id": resultMessageID}
	resp, err := c.do(http.MethodPost, "/api/agent/messages/"+messageID+"/completed", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// ReportFailure reports a failed message execution.
func (c *Client) ReportFailure(messageID string, errMsg string) error {
	payload := map[string]string{"error": errMsg}
	resp, err := c.do(http.MethodPost, "/api/agent/messages/"+messageID+"/failed", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// UpdateOffset updates the agent's message offset on the Cloud.
func (c *Client) UpdateOffset(offset int64) error {
	payload := map[string]int64{"offset": offset}
	resp, err := c.do(http.MethodPost, "/api/agent/offset", payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
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
