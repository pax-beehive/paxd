package paxlclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/pax-beehive/paxd/pkg/model"
)

type Client struct {
	Command    []string
	WorkingDir string
	Env        map[string]string
}

func (c Client) ListSessions(ctx context.Context, agent string, limit int) ([]model.SessionInfo, error) {
	args := []string{"session", "list", "--format", "jsonl"}
	if strings.TrimSpace(agent) != "" {
		args = append(args, "--agent", strings.TrimSpace(agent))
	}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("run paxl session list: %w", err)
	}
	return DecodeSessionList(out)
}

func (c Client) GetSessionElements(ctx context.Context, sessionID, agent string) ([]sessionstore.Element, error) {
	args := []string{"session", "get", "--format", "jsonl"}
	if strings.TrimSpace(agent) != "" {
		args = append(args, "--agent", strings.TrimSpace(agent))
	}
	args = append(args, sessionID)
	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("run paxl session get: %w", err)
	}
	return DecodeSessionElements(out)
}

func (c Client) GetSessionMessages(ctx context.Context, sessionID, agent string) ([]model.SessionMessage, error) {
	elements, err := c.GetSessionElements(ctx, sessionID, agent)
	if err != nil {
		return nil, err
	}
	return sessionMessages(elements), nil
}

func DecodeSessionElements(data []byte) ([]sessionstore.Element, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var elements []sessionstore.Element
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var element sessionstore.Element
		if err := json.Unmarshal(line, &element); err != nil {
			return nil, fmt.Errorf("decode paxl session element: %w", err)
		}
		if element.Type == "" {
			continue
		}
		var usage struct {
			Usage     json.RawMessage `json:"usage"`
			UsageJSON string          `json:"usageJson"`
		}
		if err := json.Unmarshal(line, &usage); err != nil {
			return nil, err
		}
		element.UsageJSON = usage.UsageJSON
		if len(usage.Usage) > 0 && string(usage.Usage) != "null" {
			element.UsageJSON = string(usage.Usage)
		}
		elements = append(elements, element)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan paxl session elements: %w", err)
	}
	return elements, nil
}

func sessionMessages(elements []sessionstore.Element) []model.SessionMessage {
	var messages []model.SessionMessage
	for _, e := range elements {
		if e.SessionID != "" && e.ContentText != "" {
			messages = append(messages, model.SessionMessage{SessionID: e.SessionID, Seq: e.Seq, Kind: e.Type, Role: e.Role, Text: e.ContentText, StartedAt: e.StartedAt, CompletedAt: e.CompletedAt})
		}
	}
	return messages
}

func (c Client) run(ctx context.Context, args ...string) ([]byte, error) {
	command := c.Command
	if len(command) == 0 {
		command = []string{"paxl"}
	}
	cmd := exec.CommandContext(ctx, command[0], append(command[1:], args...)...)
	cmd.Dir = c.WorkingDir
	cmd.Env = cmd.Environ()
	for key, value := range c.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return out, nil
}

func DecodeSessionList(data []byte) ([]model.SessionInfo, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var sessions []model.SessionInfo
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var item paxlSession
		if err := json.Unmarshal(line, &item); err != nil {
			return nil, fmt.Errorf("decode paxl session metadata: %w", err)
		}
		session := item.toSessionInfo()
		if session.SessionID != "" {
			sessions = append(sessions, session)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan paxl session metadata: %w", err)
	}
	return sessions, nil
}

func DecodeSessionMessages(data []byte) ([]model.SessionMessage, error) {
	elements, err := DecodeSessionElements(data)
	if err != nil {
		return nil, err
	}
	return sessionMessages(elements), nil
}

type paxlSession struct {
	WorkspaceRoots []string `json:"workspaceRoots"`
	ID             string   `json:"id"`
	Agent          string   `json:"agent"`
	NativeID       string   `json:"nativeId"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	Preview        string   `json:"preview"`
	ProjectID      string   `json:"projectId"`
	UpdatedAt      string   `json:"updatedAt"`
	LastSyncedAt   string   `json:"lastSyncedAt"`
}

func (s paxlSession) toSessionInfo() model.SessionInfo {
	agent := strings.TrimSpace(s.Agent)
	nativeID := strings.TrimSpace(s.NativeID)
	sessionID := strings.TrimSpace(s.ID)
	if sessionID == "" && agent != "" && nativeID != "" {
		sessionID = agent + ":" + nativeID
	}
	if nativeID == "" && strings.Contains(sessionID, ":") {
		nativeID = strings.SplitN(sessionID, ":", 2)[1]
	}
	if agent == "" && strings.Contains(sessionID, ":") {
		agent = strings.SplitN(sessionID, ":", 2)[0]
	}
	return model.SessionInfo{
		WorkspaceRoots: s.WorkspaceRoots,
		SessionID:      sessionID,
		AgentType:      agent,
		NativeID:       nativeID,
		Name:           s.Title,
		ProjectID:      s.ProjectID,
		LastActive:     s.UpdatedAt,
		Preview:        s.Preview,
		Source:         "paxl",
		Status:         s.Status,
		UpdatedAt:      s.UpdatedAt,
		CurrentTask:    "",
	}
}
