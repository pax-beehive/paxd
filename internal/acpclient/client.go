// Package acpclient contains small JSON-RPC probes for ACP stdio servers.
package acpclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/pkg/model"
)

// SessionLister lists sessions from a local ACP stdio command.
type SessionLister struct {
	Command    []string
	WorkingDir string
	Timeout    time.Duration
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int64           `json:"code,omitempty"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type initializeResult struct {
	AuthMethods []authMethod `json:"authMethods"`
}

type authMethod struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
	Vars []struct {
		Name string `json:"name"`
	} `json:"vars,omitempty"`
}

type sessionListResult struct {
	Sessions []json.RawMessage `json:"sessions"`
}

// List starts the configured ACP command, initializes it, optionally
// authenticates with the first non-terminal auth method, then calls session/list.
func (l SessionLister) List(ctx context.Context) ([]model.SessionInfo, error) {
	if len(l.Command) == 0 {
		return nil, fmt.Errorf("acp command is required")
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, l.Command[0], l.Command[1:]...)
	cmd.Dir = l.WorkingDir
	cmd.WaitDelay = 2 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start acp command: %w", err)
	}
	defer func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	nextID := int64(1)
	initResult, err := call[initializeResult](ctx, stdin, reader, nextID, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"clientInfo": map[string]any{
			"name":    "paxd-session-lister",
			"version": "0.1.0",
		},
	})
	if err != nil {
		return nil, withStderr("initialize", err, stderr.String())
	}
	nextID++

	if methodID := firstNonTerminalAuthMethod(initResult.AuthMethods); methodID != "" {
		if _, err := call[map[string]any](ctx, stdin, reader, nextID, "authenticate", map[string]any{
			"methodId": methodID,
		}); err != nil {
			return nil, withStderr("authenticate", err, stderr.String())
		}
		nextID++
	}

	listResult, err := call[sessionListResult](ctx, stdin, reader, nextID, "session/list", map[string]any{})
	if err != nil {
		return nil, withStderr("session/list", err, stderr.String())
	}

	sessions := make([]model.SessionInfo, 0, len(listResult.Sessions))
	for _, raw := range listResult.Sessions {
		session := decodeSession(raw)
		if session.SessionID != "" {
			sessions = append(sessions, session)
		}
	}
	return sessions, nil
}

func call[T any](
	ctx context.Context,
	stdin io.Writer,
	reader *bufio.Reader,
	id int64,
	method string,
	params any,
) (T, error) {
	var zero T
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return zero, err
	}
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		return zero, fmt.Errorf("write request: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		default:
		}

		line, err := readResponseLine(ctx, reader)
		if err != nil {
			return zero, fmt.Errorf("read response: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.ID != id {
			continue
		}
		if msg.Error != nil {
			return zero, fmt.Errorf("rpc error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		var out T
		if len(msg.Result) > 0 {
			if err := json.Unmarshal(msg.Result, &out); err != nil {
				return zero, fmt.Errorf("decode result: %w", err)
			}
		}
		return out, nil
	}
}

type responseLine struct {
	line []byte
	err  error
}

func readResponseLine(ctx context.Context, reader *bufio.Reader) ([]byte, error) {
	ch := make(chan responseLine, 1)
	go func() {
		line, err := reader.ReadBytes('\n')
		ch <- responseLine{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-ch:
		return result.line, result.err
	}
}

func firstNonTerminalAuthMethod(methods []authMethod) string {
	for _, method := range methods {
		if method.Type != "env_var" {
			continue
		}
		for _, envVar := range method.Vars {
			if envVar.Name != "" && os.Getenv(envVar.Name) != "" {
				return method.ID
			}
		}
	}
	for _, method := range methods {
		if method.ID != "" && !interactiveAuthMethod(method) {
			return method.ID
		}
	}
	return ""
}

func interactiveAuthMethod(method authMethod) bool {
	if method.Type == "terminal" || method.Type == "oauth" {
		return true
	}
	text := strings.ToLower(method.ID + " " + method.Name)
	return strings.Contains(text, "oauth") ||
		strings.Contains(text, "api-key") ||
		strings.Contains(text, "api key") ||
		strings.Contains(text, "gateway") ||
		strings.Contains(text, "vertex") ||
		strings.Contains(text, "setup") ||
		strings.Contains(text, "log in") ||
		strings.Contains(text, "login")
}

func decodeSession(raw json.RawMessage) model.SessionInfo {
	var typed struct {
		SessionID      string   `json:"sessionId"`
		ID             string   `json:"id"`
		AgentType      string   `json:"agentType"`
		NativeID       string   `json:"nativeId"`
		Name           string   `json:"name"`
		Title          string   `json:"title"`
		Cwd            string   `json:"cwd"`
		ProjectID      string   `json:"projectId"`
		LastActive     string   `json:"lastActive"`
		Preview        string   `json:"preview"`
		WorkspaceRoots []string `json:"workspaceRoots"`
		Status         string   `json:"status"`
		CurrentTask    string   `json:"currentTask"`
		UpdatedAt      string   `json:"updatedAt"`
	}
	_ = json.Unmarshal(raw, &typed)
	workspaceRoots := typed.WorkspaceRoots
	if len(workspaceRoots) == 0 && typed.Cwd != "" {
		workspaceRoots = []string{typed.Cwd}
	}
	return model.SessionInfo{
		SessionID:      firstNonEmpty(typed.SessionID, typed.ID),
		AgentType:      typed.AgentType,
		NativeID:       typed.NativeID,
		Name:           firstNonEmpty(typed.Name, typed.Title),
		ProjectID:      firstNonEmpty(typed.ProjectID, typed.Cwd),
		LastActive:     typed.LastActive,
		Preview:        typed.Preview,
		WorkspaceRoots: workspaceRoots,
		Status:         typed.Status,
		CurrentTask:    typed.CurrentTask,
		UpdatedAt:      typed.UpdatedAt,
	}
}

func withStderr(operation string, err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return fmt.Errorf("%s: %w", operation, err)
	}
	const maxStderr = 512
	if len(stderr) > maxStderr {
		stderr = stderr[len(stderr)-maxStderr:]
	}
	return fmt.Errorf("%s: %w: %s", operation, err, stderr)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
