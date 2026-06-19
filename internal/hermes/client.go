// Package hermes provides an HTTP client for the local Hermes API Server,
// yielding structured model.* events from SSE streaming responses.
package hermes

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/pkg/model"
)

// Client communicates with a Hermes API Server instance.
type Client struct {
	endpoint   string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new Hermes API client.
func NewClient(endpoint, apiKey string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		httpClient: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

// Ping checks if Hermes is reachable.
func (c *Client) Ping() error {
	req, err := http.NewRequest("GET", c.endpoint+"/health", nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
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
func (c *Client) GetSessions() ([]model.SessionInfo, error) {
	return c.GetSessionsContext(context.Background())
}

// GetSessionsContext fetches all active sessions from Hermes.
func (c *Client) GetSessionsContext(ctx context.Context) ([]model.SessionInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.endpoint+"/api/sessions", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get sessions: status %d: %s", resp.StatusCode, string(body))
	}
	var sessions []model.SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	return sessions, nil
}

// GetSessionStatus returns a single session's info.
// Returns nil, nil if the session is not found (treated as idle).
func (c *Client) GetSessionStatus(sessionID string) (*model.SessionInfo, error) {
	req, err := http.NewRequest("GET", c.endpoint+"/api/sessions/"+sessionID, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // not found = idle
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("get session: status %d: %s", resp.StatusCode, string(body))
	}
	var s model.SessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode session: %w", err)
	}
	return &s, nil
}

// StopRun stops a running session.
func (c *Client) StopRun(sessionID string) error {
	req, err := http.NewRequest("POST", c.endpoint+"/v1/runs/"+sessionID+"/stop", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
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

// ─── SSE Streaming with model.* events ───

// Turn contains the full result of one Hermes turn.
type Turn struct {
	SessionID   string
	TurnID      string
	ResponseID  string
	Events      []any // []*model.TurnStarted | *model.MessageDelta | *model.ToolCall | ...
	FileChanges []*model.FileChange
	Status      string // "completed" | "cancelled" | "error"
	Usage       *model.UsageInfo
}

// StreamTurn sends a prompt to Hermes and returns the full turn result.
func (c *Client) StreamTurn(ctx context.Context, prevRespID, sessionID, prompt string) (*Turn, error) {
	return c.StreamTurnLive(ctx, prevRespID, sessionID, prompt, nil)
}

// StreamTurnLive sends a prompt to Hermes, calling onEvent for each event
// as it arrives (for real-time streaming). If onEvent is nil, events are
// buffered into Turn.Events instead.
func (c *Client) StreamTurnLive(ctx context.Context, prevRespID, sessionID, prompt string, onEvent func(any)) (*Turn, error) {
	turn := &Turn{Status: "completed"}

	events := make(chan any, 64)
	errCh := make(chan error, 1)
	var realSessionID string

	go func() {
		errCh <- c.stream(ctx, prevRespID, sessionID, prompt, events, &realSessionID)
	}()

	var fileChanges []*model.FileChange
	for ev := range events {
		if onEvent != nil {
			onEvent(ev)
		} else {
			turn.Events = append(turn.Events, ev)
		}

		switch e := ev.(type) {
		case *model.TurnStarted:
			turn.TurnID = e.TurnID
		case *model.TurnDone:
			turn.ResponseID = e.ResponseID
			turn.Status = e.Status
			turn.Usage = e.Usage
		case *model.FileChanged:
			fileChanges = append(fileChanges, e.Changes...)
		case *model.TurnError:
			turn.Status = "error"
		}
	}
	turn.FileChanges = fileChanges
	turn.SessionID = realSessionID

	if err := <-errCh; err != nil {
		if turn.Status == "completed" {
			turn.Status = "error"
		}
		return turn, err
	}

	return turn, nil
}

// stream is the internal SSE parser that emits model.* events.
// Uses Hermes' OpenAI-compatible /v1/chat/completions endpoint.
// Sets *realSessionID from X-Hermes-Session-Id response header.
func (c *Client) stream(ctx context.Context, prevRespID, sessionID, prompt string, events chan<- any, realSessionID *string) error {
	defer close(events)

	// Build OpenAI-compatible payload
	type chatMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type chatRequest struct {
		Model    string        `json:"model"`
		Messages []chatMessage `json:"messages"`
		Stream   bool          `json:"stream"`
	}
	payload, _ := json.Marshal(chatRequest{
		Model:    "deepseek-v4-pro",
		Messages: []chatMessage{{Role: "user", Content: prompt}},
		Stream:   true,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		events <- model.NewAgentStatus(sessionID, "", "error", "Error", "❌", err.Error())
		events <- model.NewTurnDone(sessionID, "", "", "error")
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if sessionID != "" {
		req.Header.Set("X-Hermes-Session-Id", sessionID)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		events <- model.NewAgentStatus(sessionID, "", "error", "Error", "❌", err.Error())
		events <- model.NewTurnDone(sessionID, "", "", "error")
		return err
	}
	defer resp.Body.Close()

	// Capture Hermes session ID from response header
	if sid := resp.Header.Get("X-Hermes-Session-Id"); sid != "" {
		*realSessionID = sid
		sessionID = sid // use real session ID for all events
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := fmt.Sprintf("http %d: %s", resp.StatusCode, string(body))
		events <- model.NewAgentStatus(sessionID, "", "error", "Error", "❌", msg)
		events <- model.NewTurnDone(sessionID, "", "", "error")
		return fmt.Errorf(msg)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var turnID string
	var pendingText strings.Builder
	turnStarted := false
	var fileChanges []*model.FileChange
	var currentEvent string // SSE event: field

	flushText := func() {
		if pendingText.Len() > 0 {
			events <- model.NewMessageDelta(sessionID, turnID, "assistant", pendingText.String())
			pendingText.Reset()
		}
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			flushText()
			events <- model.NewAgentStatus(sessionID, turnID, "cancelled", "Cancelled", "⏹️", "")
			if len(fileChanges) > 0 {
				events <- model.NewFileChanged(sessionID, turnID, fileChanges)
			}
			events <- model.NewTurnDone(sessionID, turnID, "", "cancelled")
			return ctx.Err()
		default:
		}

		line := strings.TrimSpace(scanner.Text())

		// Track event: field for custom SSE events
		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		// Empty line = event boundary, reset event type
		if line == "" {
			currentEvent = ""
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		// Hermes custom event: tool progress
		if currentEvent == "hermes.tool.progress" {
			var tp toolProgressEvent
			if err := json.Unmarshal([]byte(data), &tp); err != nil {
				currentEvent = ""
				continue
			}
			switch tp.Status {
			case "running":
				flushText()
				label := fmt.Sprintf("%s %s %s", tp.Emoji, tp.Tool, tp.Label)
				events <- model.NewAgentStatus(sessionID, turnID, "working", "Working…", "🔧", label)
				events <- model.NewToolCall(sessionID, turnID, tp.ToolCallID, tp.Tool, tp.Label)

				fc := extractFileChange(tp.Tool, tp.Label)
				if fc != nil {
					fileChanges = append(fileChanges, fc)
				}

			case "completed":
				// Tool finished — no output details, just mark working → thinking
				events <- model.NewAgentStatus(sessionID, turnID, "thinking", "Thinking…", "🧠", "")

			case "failed":
				events <- model.NewAgentStatus(sessionID, turnID, "thinking", "Thinking…", "🧠", "tool failed: "+tp.Tool)
			}
			currentEvent = ""
			continue
		}

		// OpenAI-compatible chunk
		var chunk openAIChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// Start of turn on first chunk
		if !turnStarted && chunk.ID != "" {
			turnID = chunk.ID
			turnStarted = true
			events <- model.NewTurnStarted(sessionID, turnID)
			events <- model.NewAgentStatus(sessionID, turnID, "thinking", "Thinking…", "🧠", "")
		}

		for _, choice := range chunk.Choices {
			// Text delta — send immediately for real-time streaming
			if choice.Delta.Content != "" {
				events <- model.NewMessageDelta(sessionID, turnID, "assistant", choice.Delta.Content)
			}

			// OpenAI tool calls (fallback for non-Hermes backends)
			if len(choice.Delta.ToolCalls) > 0 {
				flushText()
				for _, tc := range choice.Delta.ToolCalls {
					if tc.Function.Name != "" {
						label := formatToolLabel(tc.Function.Name, tc.Function.Arguments)
						events <- model.NewAgentStatus(sessionID, turnID, "working", "Working…", "🔧", label)
						events <- model.NewToolCall(sessionID, turnID, tc.ID, tc.Function.Name, tc.Function.Arguments)

						fc := extractFileChange(tc.Function.Name, tc.Function.Arguments)
						if fc != nil {
							fileChanges = append(fileChanges, fc)
						}
					}
				}
			}

			// Finish reason
			if choice.FinishReason == "tool_calls" {
				flushText()
			}
		}
	}

	if err := scanner.Err(); err != nil {
		events <- model.NewAgentStatus(sessionID, turnID, "error", "Error", "❌", err.Error())
		events <- model.NewTurnDone(sessionID, turnID, "", "error")
		return err
	}

	flushText()

	if turnStarted {
		events <- model.NewAgentStatus(sessionID, turnID, "done", "Done", "✅", "")
		if len(fileChanges) > 0 {
			events <- model.NewFileChanged(sessionID, turnID, fileChanges)
		}
		events <- model.NewTurnDone(sessionID, turnID, turnID, "completed")
	}
	events <- model.NewAgentStatus(sessionID, "", "idle", "", "", "")

	return nil
}

// ─── Hermes tool progress SSE types ───

type toolProgressEvent struct {
	Tool       string `json:"tool"`
	Emoji      string `json:"emoji"`
	Label      string `json:"label"`
	ToolCallID string `json:"toolCallId"`
	Status     string `json:"status"` // "running" | "completed" | "failed"
}

// ─── OpenAI SSE types ───

type openAIChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// ─── Helpers ───

func extractFileChange(toolName, rawArgs string) *model.FileChange {
	if toolName != "write_file" && toolName != "patch" {
		return nil
	}
	var args map[string]any
	if json.Unmarshal([]byte(rawArgs), &args) != nil {
		return nil
	}
	path, _ := args["path"].(string)
	if path == "" {
		return nil
	}
	fc := &model.FileChange{Path: path, Tool: toolName}
	if toolName == "write_file" {
		fc.NewContent, _ = args["content"].(string)
	} else {
		fc.OldContent, _ = args["old_string"].(string)
		fc.NewContent, _ = args["new_string"].(string)
	}
	return fc
}

var toolEmojis = map[string]string{
	"terminal": "💻", "shell_exec": "💻", "web_search": "🔍", "web_extract": "📄",
	"read_file": "📖", "write_file": "✏️", "patch": "🩹", "search_files": "🔎",
	"browser_navigate": "🌐", "browser_click": "🖱️", "browser_type": "⌨️",
	"execute_code": "🐍", "todo": "📋", "delegate_task": "🤖", "memory": "🧠",
}

var toolPrimaryKey = map[string]string{
	"terminal": "command", "web_search": "query", "read_file": "path",
	"write_file": "path", "patch": "path", "search_files": "pattern",
	"browser_navigate": "url", "browser_click": "ref", "browser_type": "text",
	"execute_code": "code", "delegate_task": "goal",
}

func formatToolLabel(name, rawArgs string) string {
	var args map[string]any
	json.Unmarshal([]byte(rawArgs), &args)
	emoji := toolEmojis[name]
	if emoji == "" {
		emoji = "🔧"
	}
	key := toolPrimaryKey[name]
	detail := ""
	if val, ok := args[key]; ok {
		detail = fmt.Sprint(val)
		if len(detail) > 80 {
			detail = detail[:80] + "..."
		}
	}
	if detail != "" {
		return fmt.Sprintf("%s %s %s", emoji, name, detail)
	}
	return fmt.Sprintf("%s %s", emoji, name)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
