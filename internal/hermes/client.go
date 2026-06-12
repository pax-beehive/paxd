// Package hermes provides an HTTP client for the local Hermes API Server.
package hermes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client communicates with a Hermes API Server instance.
type Client struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client
}

// SessionInfo represents a Hermes session as returned by the API.
type SessionInfo struct {
	SessionID   string `json:"session_id"`
	Status      string `json:"status"`       // "idle", "running", "completed"
	CurrentTask string `json:"current_task"`
	TokenUsage  int64  `json:"token_usage"`
	UpdatedAt   string `json:"updated_at"`
}

// ChatRequest is sent to Hermes chat/stream endpoint.
type ChatRequest struct {
	SessionID string `json:"session_id,omitempty"` // empty = create new
	Message   string `json:"message"`
	Stream    bool   `json:"stream"`
}

// ChatEvent is a streaming SSE event from Hermes.
type ChatEvent struct {
	Type    string `json:"type"`    // "text", "tool_call", "done", "error"
	Content string `json:"content"`
}

// NewClient creates a new Hermes API client.
func NewClient(endpoint, apiKey string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute, // streaming responses can be long
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

	req, err := http.NewRequest(method, c.endpoint+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	return c.httpClient.Do(req)
}

// Ping checks if Hermes is reachable.
func (c *Client) Ping() error {
	resp, err := c.do(http.MethodGet, "/health", nil)
	if err != nil {
		return fmt.Errorf("hermes unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hermes unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// GetSessions fetches all active sessions from Hermes.
func (c *Client) GetSessions() ([]SessionInfo, error) {
	resp, err := c.do(http.MethodGet, "/api/sessions", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get sessions: status %d: %s", resp.StatusCode, string(body))
	}

	var sessions []SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	return sessions, nil
}

// GetSessionStatus returns a single session's status.
func (c *Client) GetSessionStatus(sessionID string) (*SessionInfo, error) {
	resp, err := c.do(http.MethodGet, "/api/sessions/"+sessionID, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get session: status %d: %s", resp.StatusCode, string(body))
	}

	var s SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode session: %w", err)
	}
	return &s, nil
}

// ChatStream sends a chat message and streams the response via callback.
// Returns the full accumulated response text.
func (c *Client) ChatStream(sessionID, message string, onEvent func(ChatEvent)) (string, error) {
	req := ChatRequest{
		SessionID: sessionID,
		Message:   message,
		Stream:    true,
	}

	path := "/api/sessions/" + sessionID + "/chat/stream"
	if sessionID == "" {
		path = "/api/chat/stream"
	}

	resp, err := c.do(http.MethodPost, path, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chat stream: status %d: %s", resp.StatusCode, string(body))
	}

	return parseSSE(resp.Body, onEvent)
}

// StopRun stops a running session.
func (c *Client) StopRun(sessionID string) error {
	resp, err := c.do(http.MethodPost, "/v1/runs/"+sessionID+"/stop", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("stop run: status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// parseSSE reads Server-Sent Events from the response and calls onEvent for each.
// Returns the concatenated text content.
func parseSSE(r io.Reader, onEvent func(ChatEvent)) (string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var fullText strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			if data == "[DONE]" {
				break
			}
			var event ChatEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				continue // skip unparseable events
			}
			if onEvent != nil {
				onEvent(event)
			}
			if event.Type == "text" {
				fullText.WriteString(event.Content)
			}
			if event.Type == "error" {
				return fullText.String(), fmt.Errorf("hermes error: %s", event.Content)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fullText.String(), fmt.Errorf("read SSE: %w", err)
	}
	return fullText.String(), nil
}
