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
	req, err := http.NewRequest("GET", c.endpoint+"/api/sessions", nil)
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
	TurnID      string
	ResponseID  string
	Events      []any            // []*model.TurnStarted | *model.MessageDelta | *model.ToolCall | ...
	FileChanges []*model.FileChange
	Status      string           // "completed" | "cancelled" | "error"
	Usage       *model.UsageInfo
}

// StreamTurn sends a prompt to Hermes and returns the full turn result.
func (c *Client) StreamTurn(ctx context.Context, prevRespID, sessionID, prompt string) (*Turn, error) {
	turn := &Turn{Status: "completed"}

	events := make(chan any, 64)
	errCh := make(chan error, 1)

	go func() {
		errCh <- c.stream(ctx, prevRespID, sessionID, prompt, events)
	}()

	var fileChanges []*model.FileChange
	for ev := range events {
		turn.Events = append(turn.Events, ev)

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

	if err := <-errCh; err != nil {
		if turn.Status == "completed" {
			turn.Status = "error"
		}
		return turn, err
	}

	return turn, nil
}

// stream is the internal SSE parser that emits model.* events.
func (c *Client) stream(ctx context.Context, prevRespID, sessionID, prompt string, events chan<- any) error {
	defer close(events)

	body := struct {
		Input              string `json:"input"`
		PreviousResponseID string `json:"previous_response_id,omitempty"`
		Stream             bool   `json:"stream"`
		Store              bool   `json:"store"`
	}{Input: prompt, PreviousResponseID: prevRespID, Stream: true, Store: true}

	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(payload))
	if err != nil {
		events <- model.NewAgentStatus("", "error", "Error", "❌", err.Error())
		events <- model.NewTurnDone("", "", "error")
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		events <- model.NewAgentStatus("", "error", "Error", "❌", err.Error())
		events <- model.NewTurnDone("", "", "error")
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := fmt.Sprintf("http %d: %s", resp.StatusCode, string(body))
		events <- model.NewAgentStatus("", "error", "Error", "❌", msg)
		events <- model.NewTurnDone("", "", "error")
		return fmt.Errorf(msg)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

	var turnID string
	var pendingText strings.Builder
	turnStarted := false
	var fileChanges []*model.FileChange

	flushText := func() {
		if pendingText.Len() > 0 {
			events <- model.NewMessageDelta(turnID, "assistant", pendingText.String())
			pendingText.Reset()
		}
	}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			flushText()
			events <- model.NewAgentStatus(turnID, "cancelled", "Cancelled", "⏹️", "")
			if len(fileChanges) > 0 {
				events <- model.NewFileChanged(turnID, fileChanges)
			}
			events <- model.NewTurnDone(turnID, "", "cancelled")
			return ctx.Err()
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var sse hermesSSEvent
		if err := json.Unmarshal([]byte(data), &sse); err != nil {
			continue
		}

		switch sse.Type {
		case "response.created":
			turnID = sse.Response.ID
			turnStarted = true
			events <- model.NewTurnStarted(turnID)
			events <- model.NewAgentStatus(turnID, "thinking", "Thinking…", "🧠", "")

		case "response.output_text.delta":
			pendingText.WriteString(sse.Delta)

		case "response.output_item.added":
			if sse.Item.Type == "function_call" {
				flushText()
				label := formatToolLabel(sse.Item.Name, sse.Item.Arguments)
				events <- model.NewAgentStatus(turnID, "working", "Working…", "🔧", label)
				events <- model.NewToolCall(turnID, sse.Item.CallID, sse.Item.Name, sse.Item.Arguments)

				// Track file changes from write_file / patch
				fc := extractFileChange(sse.Item.Name, sse.Item.Arguments)
				if fc != nil {
					fileChanges = append(fileChanges, fc)
				}
			}
			if sse.Item.Type == "function_call_output" {
				output := extractToolOutput(sse.Item.Output)
				events <- model.NewToolResult(turnID, sse.Item.CallID, output)
				events <- model.NewMessageDelta(turnID, "tool", "┊ "+truncate(output, 200))
				events <- model.NewAgentStatus(turnID, "thinking", "Thinking…", "🧠", "")
			}

		case "response.output_text.done":
			flushText()

		case "response.completed":
			// handled after loop
		}
	}

	if err := scanner.Err(); err != nil {
		events <- model.NewAgentStatus(turnID, "error", "Error", "❌", err.Error())
		events <- model.NewTurnDone(turnID, "", "error")
		return err
	}

	flushText()

	if turnStarted {
		events <- model.NewAgentStatus(turnID, "done", "Done", "✅", "")
		if len(fileChanges) > 0 {
			events <- model.NewFileChanged(turnID, fileChanges)
		}
		events <- model.NewTurnDone(turnID, turnID, "completed")
	}
	events <- model.NewAgentStatus("", "idle", "", "", "")

	return nil
}

// ─── SSE internal types ───

type hermesSSEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Item     struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		CallID    string `json:"call_id"`
		Output    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"output"`
	} `json:"item"`
	Response struct {
		ID string `json:"id"`
	} `json:"response"`
}

// ─── Helpers ───

func extractToolOutput(outputs []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	for _, o := range outputs {
		if o.Type == "input_text" {
			return o.Text
		}
	}
	return ""
}

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
