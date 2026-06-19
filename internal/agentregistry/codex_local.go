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
	"time"

	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/pax-beehive/paxd/pkg/model"
)

type codexIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
	UpdatedAt  string `json:"updated_at"`
}

type codexSessionMetaLine struct {
	Type    string `json:"type"`
	Payload struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
		CWD       string `json:"cwd"`
		Source    string `json:"source"`
	} `json:"payload"`
}

func codexLocalAvailable() bool {
	root, err := codexRoot()
	if err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(root, "sessions")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(root, "session_index.jsonl")); err == nil {
		return true
	}
	return false
}

func listCodexLocalSessions() ([]model.SessionInfo, error) {
	root, err := codexRoot()
	if err != nil {
		return nil, err
	}
	byID, err := readCodexSessionIndex(filepath.Join(root, "session_index.jsonl"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := readCodexRollouts(filepath.Join(root, "sessions"), byID); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	out := make([]model.SessionInfo, 0, len(byID))
	for _, session := range byID {
		out = append(out, session)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out, nil
}

func codexRoot() (string, error) {
	if value := os.Getenv("CODEX_HOME"); value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func readCodexSessionIndex(path string) (map[string]model.SessionInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return map[string]model.SessionInfo{}, err
	}
	defer file.Close()

	out := map[string]model.SessionInfo{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry codexIndexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.ID == "" {
			continue
		}
		out[entry.ID] = model.SessionInfo{
			SessionID:  "codex:" + entry.ID,
			AgentType:  "codex",
			NativeID:   entry.ID,
			Name:       entry.ThreadName,
			UpdatedAt:  entry.UpdatedAt,
			LastActive: entry.UpdatedAt,
			Status:     "available",
		}
	}
	return out, scanner.Err()
}

func readCodexRollouts(root string, byID map[string]model.SessionInfo) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		meta, err := readCodexRolloutMeta(path)
		if err != nil || meta.Payload.ID == "" {
			return nil
		}
		session := byID[meta.Payload.ID]
		session.SessionID = "codex:" + meta.Payload.ID
		session.AgentType = "codex"
		session.NativeID = meta.Payload.ID
		if session.Name == "" {
			session.Name = firstNonEmpty(meta.Payload.Source, entry.Name())
		}
		if session.UpdatedAt == "" {
			session.UpdatedAt = meta.Payload.Timestamp
		}
		if session.LastActive == "" {
			session.LastActive = session.UpdatedAt
		}
		if meta.Payload.CWD != "" {
			session.ProjectID = meta.Payload.CWD
			session.WorkspaceRoots = []string{meta.Payload.CWD}
		}
		session.Status = firstNonEmpty(session.Status, "available")
		byID[meta.Payload.ID] = session
		return nil
	})
}

func readCodexRolloutMeta(path string) (codexSessionMetaLine, error) {
	file, err := os.Open(path)
	if err != nil {
		return codexSessionMetaLine{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var line codexSessionMetaLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}
		if line.Type == "session_meta" {
			return line, nil
		}
	}
	return codexSessionMetaLine{}, scanner.Err()
}

func CodexLocalElements(nativeID string) ([]sessionstore.Element, error) {
	path, err := findCodexRollout(nativeID)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var elements []sessionstore.Element
	durations := map[string]int64{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if callID, durationMS, ok := codexToolDuration(line); ok {
			durations[callID] = durationMS
		}
		element, ok := decodeCodexElement(line, int64(len(elements)+1), "codex:"+nativeID)
		if ok {
			elements = append(elements, element)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return enrichCodexToolDurations(elements, durations), nil
}

func findCodexRollout(nativeID string) (string, error) {
	root, err := codexRoot()
	if err != nil {
		return "", err
	}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		found, err := findCodexRolloutInDir(filepath.Join(root, dir), nativeID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf("codex rollout %q not found under ~/.codex/sessions or ~/.codex/archived_sessions", nativeID)
}

func findCodexRolloutInDir(root, nativeID string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if found != "" || entry.IsDir() {
			return nil
		}
		if strings.HasPrefix(entry.Name(), "rollout-") && strings.HasSuffix(entry.Name(), nativeID+".jsonl") {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return found, nil
}

func decodeCodexElement(line []byte, seq int64, sessionID string) (sessionstore.Element, bool) {
	var envelope struct {
		Timestamp string          `json:"timestamp"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return sessionstore.Element{}, false
	}
	if envelope.Type == "event_msg" {
		return decodeCodexEventMessage(line, envelope, seq, sessionID)
	}
	if envelope.Type != "response_item" {
		return sessionstore.Element{}, false
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(envelope.Payload, &kind); err != nil {
		return sessionstore.Element{}, false
	}
	element := sessionstore.Element{
		SessionID:   sessionID,
		Seq:         seq,
		StartedAt:   envelope.Timestamp,
		CompletedAt: envelope.Timestamp,
		RawJSON:     string(line),
	}
	switch kind.Type {
	case "message":
		var payload struct {
			Role    string `json:"role"`
			Content []struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				InputText  string `json:"input_text"`
				OutputText string `json:"output_text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(envelope.Payload, &payload)
		element.Type = "message"
		element.Role = payload.Role
		element.ContentText = codexContentText(payload.Content)
	case "reasoning":
		var payload struct {
			Summary          []any  `json:"summary"`
			EncryptedContent string `json:"encrypted_content"`
			Model            string `json:"model"`
			Metadata         any    `json:"metadata"`
		}
		_ = json.Unmarshal(envelope.Payload, &payload)
		element.Type = "thinking"
		element.Model = payload.Model
		if len(payload.Summary) > 0 {
			summary, _ := json.MarshalIndent(payload.Summary, "", "  ")
			element.ContentText = string(summary)
		} else if payload.EncryptedContent != "" {
			element.ContentText = "[encrypted reasoning content]"
		}
	case "function_call":
		var payload struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			CallID    string `json:"call_id"`
		}
		_ = json.Unmarshal(envelope.Payload, &payload)
		element.Type = "tool_call"
		element.ContentText = fmt.Sprintf("%s %s\n%s", payload.Name, payload.CallID, payload.Arguments)
		element.NormalizedRaw = map[string]any{
			"name":      payload.Name,
			"callId":    payload.CallID,
			"arguments": decodeJSONValue(payload.Arguments),
		}
	case "function_call_output":
		var payload struct {
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}
		_ = json.Unmarshal(envelope.Payload, &payload)
		element.Type = "tool_result"
		element.ContentText = fmt.Sprintf("%s\n%s", payload.CallID, payload.Output)
		element.NormalizedRaw = map[string]any{
			"callId": payload.CallID,
			"output": decodeJSONValue(payload.Output),
		}
	default:
		return sessionstore.Element{}, false
	}
	if strings.TrimSpace(element.ContentText) == "" {
		return sessionstore.Element{}, false
	}
	return element, true
}

func decodeCodexEventMessage(line []byte, envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}, seq int64, sessionID string) (sessionstore.Element, bool) {
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(envelope.Payload, &kind); err != nil {
		return sessionstore.Element{}, false
	}
	if kind.Type != "token_count" {
		return sessionstore.Element{}, false
	}
	var payload struct {
		Info json.RawMessage `json:"info"`
	}
	_ = json.Unmarshal(envelope.Payload, &payload)
	if len(payload.Info) == 0 {
		return sessionstore.Element{}, false
	}
	element := sessionstore.Element{
		SessionID:     sessionID,
		Seq:           seq,
		Type:          "usage",
		StartedAt:     envelope.Timestamp,
		CompletedAt:   envelope.Timestamp,
		UsageJSON:     string(payload.Info),
		ContentText:   tokenUsageSummary(payload.Info),
		NormalizedRaw: map[string]any{"kind": "token_count"},
		RawJSON:       string(line),
	}
	return element, true
}

func tokenUsageSummary(raw json.RawMessage) string {
	var decoded struct {
		Total struct {
			Input     int64 `json:"input_tokens"`
			Cached    int64 `json:"cached_input_tokens"`
			Output    int64 `json:"output_tokens"`
			Reasoning int64 `json:"reasoning_output_tokens"`
			Total     int64 `json:"total_tokens"`
		} `json:"total_token_usage"`
		Last struct {
			Input     int64 `json:"input_tokens"`
			Cached    int64 `json:"cached_input_tokens"`
			Output    int64 `json:"output_tokens"`
			Reasoning int64 `json:"reasoning_output_tokens"`
			Total     int64 `json:"total_tokens"`
		} `json:"last_token_usage"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	return fmt.Sprintf("last total=%d input=%d cached=%d output=%d reasoning=%d\nsession total=%d input=%d cached=%d output=%d reasoning=%d",
		decoded.Last.Total, decoded.Last.Input, decoded.Last.Cached, decoded.Last.Output, decoded.Last.Reasoning,
		decoded.Total.Total, decoded.Total.Input, decoded.Total.Cached, decoded.Total.Output, decoded.Total.Reasoning)
}

func enrichCodexToolDurations(elements []sessionstore.Element, durations map[string]int64) []sessionstore.Element {
	for idx := range elements {
		if elements[idx].DurationMS > 0 {
			continue
		}
		callID, ok := codexStringField(elements[idx].NormalizedRaw, "callId")
		if !ok {
			continue
		}
		if durationMS := durations[callID]; durationMS > 0 {
			elements[idx].DurationMS = durationMS
		}
	}
	return elements
}

func codexStringField(values map[string]any, key string) (string, bool) {
	value, ok := values[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok && text != ""
}

func codexToolDuration(raw []byte) (string, int64, bool) {
	var envelope struct {
		Type    string `json:"type"`
		Payload struct {
			Type     string `json:"type"`
			CallID   string `json:"call_id"`
			Duration struct {
				Secs  int64 `json:"secs"`
				Nanos int64 `json:"nanos"`
			} `json:"duration"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", 0, false
	}
	if envelope.Type != "event_msg" || envelope.Payload.CallID == "" {
		return "", 0, false
	}
	durationMS := envelope.Payload.Duration.Secs*1000 + envelope.Payload.Duration.Nanos/int64(time.Millisecond)
	if durationMS <= 0 {
		return "", 0, false
	}
	return envelope.Payload.CallID, durationMS, true
}

func decodeJSONValue(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err == nil {
		return decoded
	}
	return value
}

func codexContentText(parts []struct {
	Type       string `json:"type"`
	Text       string `json:"text"`
	InputText  string `json:"input_text"`
	OutputText string `json:"output_text"`
}) string {
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		text := firstNonEmpty(part.Text, part.InputText, part.OutputText)
		if text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}
