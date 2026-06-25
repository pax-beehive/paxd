package agentregistry

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/pax-beehive/paxd/pkg/model"
)

type qwenChatLine struct {
	UUID          string          `json:"uuid"`
	ParentUUID    string          `json:"parentUuid"`
	SessionID     string          `json:"sessionId"`
	Timestamp     string          `json:"timestamp"`
	Type          string          `json:"type"`
	CWD           string          `json:"cwd"`
	Version       string          `json:"version"`
	GitBranch     string          `json:"gitBranch"`
	Model         string          `json:"model"`
	Subtype       string          `json:"subtype"`
	Message       qwenMessage     `json:"message"`
	SystemPayload json.RawMessage `json:"systemPayload"`
	UsageMetadata qwenUsage       `json:"usageMetadata"`
	ContextWindow int64           `json:"contextWindowSize"`
}

type qwenMessage struct {
	Role  string `json:"role"`
	Parts []struct {
		Text string `json:"text"`
	} `json:"parts"`
}

type qwenUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

func qwenLocalAvailable() bool {
	root, err := qwenRoot()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, "projects")); err == nil {
		return true
	}
	return false
}

func qwenRoot() (string, error) {
	if value := os.Getenv("QWEN_HOME"); value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".qwen"), nil
}

func listQwenLocalSessions() ([]model.SessionInfo, error) {
	paths, err := qwenChatPaths()
	if err != nil {
		return nil, err
	}
	sessions := make([]model.SessionInfo, 0, len(paths))
	for _, path := range paths {
		session, err := readQwenSessionMeta(path)
		if err == nil && session.SessionID != "" {
			sessions = append(sessions, session)
		}
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt > sessions[j].UpdatedAt
	})
	return sessions, nil
}

func qwenChatPaths() ([]string, error) {
	root, err := qwenRoot()
	if err != nil {
		return nil, err
	}
	projectRoot := filepath.Join(root, "projects")
	var paths []string
	err = filepath.WalkDir(projectRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") || strings.HasSuffix(entry.Name(), ".runtime.json") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "chats" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

func readQwenSessionMeta(path string) (model.SessionInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return model.SessionInfo{}, err
	}
	defer file.Close()

	var first, last qwenChatLine
	var title, preview string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var line qwenChatLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil || line.SessionID == "" {
			continue
		}
		if first.SessionID == "" {
			first = line
		}
		last = line
		text := qwenMessageText(line.Message)
		if title == "" && line.Type == "user" && text != "" {
			title = trimOneLine(text, 80)
		}
		if text != "" {
			preview = trimOneLine(text, 160)
		}
	}
	if err := scanner.Err(); err != nil {
		return model.SessionInfo{}, err
	}
	if first.SessionID == "" {
		return model.SessionInfo{}, errors.New("qwen chat has no session id")
	}
	nativeID := first.SessionID
	cwd := firstNonEmpty(last.CWD, first.CWD, qwenProjectFromPath(path))
	updatedAt := firstNonEmpty(last.Timestamp, first.Timestamp)
	return model.SessionInfo{
		SessionID:      "qwen:" + nativeID,
		AgentType:      "qwen",
		NativeID:       nativeID,
		Name:           firstNonEmpty(title, nativeID),
		ProjectID:      cwd,
		LastActive:     updatedAt,
		Preview:        preview,
		WorkspaceRoots: nonEmptySlice(cwd),
		Status:         "available",
		UpdatedAt:      updatedAt,
	}, nil
}

func QwenLocalElements(nativeID string) ([]sessionstore.Element, error) {
	path, err := findQwenChat(nativeID)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var elements []sessionstore.Element
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		element, ok := decodeQwenElement(scanner.Bytes(), int64(len(elements)+1), "qwen:"+nativeID)
		if ok {
			elements = append(elements, element)
		}
	}
	return elements, scanner.Err()
}

func findQwenChat(nativeID string) (string, error) {
	paths, err := qwenChatPaths()
	if err != nil {
		return "", err
	}
	for _, path := range paths {
		if strings.TrimSuffix(filepath.Base(path), ".jsonl") == nativeID {
			return path, nil
		}
	}
	return "", fmt.Errorf("qwen chat %q not found under ~/.qwen/projects", nativeID)
}

func decodeQwenElement(raw []byte, seq int64, sessionID string) (sessionstore.Element, bool) {
	var line qwenChatLine
	if err := json.Unmarshal(raw, &line); err != nil || line.SessionID == "" {
		return sessionstore.Element{}, false
	}
	element := sessionstore.Element{
		SessionID:   sessionID,
		Seq:         seq,
		StartedAt:   line.Timestamp,
		CompletedAt: line.Timestamp,
		Model:       line.Model,
		RawJSON:     string(raw),
	}
	switch line.Type {
	case "user", "assistant":
		element.Type = "message"
		element.Role = line.Type
		if line.Type == "assistant" && line.Message.Role != "" {
			element.Role = "assistant"
		}
		element.ContentText = qwenMessageText(line.Message)
		if line.UsageMetadata.TotalTokenCount > 0 {
			element.UsageJSON = qwenUsageJSON(line.UsageMetadata)
		}
	case "system":
		usageElement, ok := qwenTelemetryUsage(line, element)
		if ok {
			return usageElement, true
		}
		element.Type = "event"
		element.Role = "system"
		element.ContentText = firstNonEmpty(line.Subtype, "system")
	default:
		return sessionstore.Element{}, false
	}
	if strings.TrimSpace(element.ContentText) == "" {
		return sessionstore.Element{}, false
	}
	return element, true
}

func qwenTelemetryUsage(line qwenChatLine, base sessionstore.Element) (sessionstore.Element, bool) {
	if line.Subtype != "ui_telemetry" || len(line.SystemPayload) == 0 {
		return sessionstore.Element{}, false
	}
	var payload struct {
		UIEvent struct {
			Model      string `json:"model"`
			DurationMS int64  `json:"duration_ms"`
			Input      int64  `json:"input_token_count"`
			Output     int64  `json:"output_token_count"`
			Cached     int64  `json:"cached_content_token_count"`
			Thoughts   int64  `json:"thoughts_token_count"`
			Total      int64  `json:"total_token_count"`
			Response   string `json:"response_text"`
		} `json:"uiEvent"`
	}
	if err := json.Unmarshal(line.SystemPayload, &payload); err != nil || payload.UIEvent.Total == 0 {
		return sessionstore.Element{}, false
	}
	usage := qwenUsage{
		PromptTokenCount:        payload.UIEvent.Input,
		CandidatesTokenCount:    payload.UIEvent.Output,
		ThoughtsTokenCount:      payload.UIEvent.Thoughts,
		TotalTokenCount:         payload.UIEvent.Total,
		CachedContentTokenCount: payload.UIEvent.Cached,
	}
	base.Type = "usage"
	base.Model = payload.UIEvent.Model
	base.DurationMS = payload.UIEvent.DurationMS
	base.UsageJSON = qwenUsageJSON(usage)
	base.ContentText = fmt.Sprintf("total=%d input=%d cached=%d output=%d thoughts=%d",
		usage.TotalTokenCount, usage.PromptTokenCount, usage.CachedContentTokenCount,
		usage.CandidatesTokenCount, usage.ThoughtsTokenCount)
	return base, true
}

func qwenMessageText(message qwenMessage) string {
	parts := make([]string, 0, len(message.Parts))
	for _, part := range message.Parts {
		if part.Text != "" {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func qwenUsageJSON(usage qwenUsage) string {
	payload, _ := json.Marshal(map[string]any{
		"input_tokens":    usage.PromptTokenCount,
		"cached_tokens":   usage.CachedContentTokenCount,
		"output_tokens":   usage.CandidatesTokenCount,
		"thoughts_tokens": usage.ThoughtsTokenCount,
		"total_tokens":    usage.TotalTokenCount,
	})
	return string(payload)
}

func qwenProjectFromPath(path string) string {
	projectDir := filepath.Dir(filepath.Dir(path))
	return strings.TrimPrefix(filepath.Base(projectDir), "-")
}

func trimOneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func nonEmptySlice(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}
