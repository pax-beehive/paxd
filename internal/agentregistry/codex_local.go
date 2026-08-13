package agentregistry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		ID             string          `json:"id"`
		SessionID      string          `json:"session_id"`
		ForkedFromID   string          `json:"forked_from_id"`
		ParentThreadID string          `json:"parent_thread_id"`
		ThreadSource   string          `json:"thread_source"`
		Timestamp      string          `json:"timestamp"`
		CWD            string          `json:"cwd"`
		Source         json.RawMessage `json:"source"`
	} `json:"payload"`
}

// isSubagent reports whether the rollout belongs to an internal subagent
// (guardian reviewers, thread spawns) rather than a user conversation.
func (m codexSessionMetaLine) isSubagent() bool {
	if m.Payload.ThreadSource == "subagent" {
		return true
	}
	var source struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if err := json.Unmarshal(m.Payload.Source, &source); err == nil && len(source.Subagent) > 0 {
		return true
	}
	return false
}

// codexRolloutMeta is the parsed session_meta line of one rollout file.
type codexRolloutMeta struct {
	path       string
	mtime      time.Time
	activityAt string
	meta       codexSessionMetaLine
}

// codexLineage resolves codex thread/rollout IDs to the root thread ID of
// their conversation. Codex assigns a conversation a new thread ID when it
// forks (for example when resuming after an auto compact); the new rollout
// records the parent thread in forked_from_id.
type codexLineage struct {
	parentOf map[string]string
}

func newCodexLineage(metas []codexRolloutMeta) codexLineage {
	parentOf := make(map[string]string, len(metas))
	for _, rollout := range metas {
		id := rollout.meta.Payload.ID
		parent := firstNonEmpty(rollout.meta.Payload.ForkedFromID, rollout.meta.Payload.ParentThreadID)
		if id == "" || parent == "" || id == parent {
			continue
		}
		parentOf[id] = parent
	}
	return codexLineage{parentOf: parentOf}
}

func (l codexLineage) rootOf(id string) string {
	seen := map[string]bool{}
	for {
		parent, ok := l.parentOf[id]
		if !ok || parent == "" || seen[id] {
			return id
		}
		seen[id] = true
		id = parent
	}
}

// conversationKey maps a rollout to the root thread ID of its conversation.
func (l codexLineage) conversationKey(meta codexSessionMetaLine) string {
	if meta.Payload.ForkedFromID != "" {
		return l.rootOf(meta.Payload.ForkedFromID)
	}
	if meta.Payload.SessionID != "" {
		return l.rootOf(meta.Payload.SessionID)
	}
	return l.rootOf(meta.Payload.ID)
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
	entries, err := readCodexSessionIndex(filepath.Join(root, "session_index.jsonl"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	metas, err := readCodexRolloutMetas(filepath.Join(root, "sessions"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return mergeCodexSessions(entries, metas), nil
}

func mergeCodexSessions(entries []codexIndexEntry, metas []codexRolloutMeta) []model.SessionInfo {
	lineage := newCodexLineage(metas)
	byRoot := make(map[string]model.SessionInfo)
	nameUpdatedAt := make(map[string]string)
	sessionFor := func(key string) model.SessionInfo {
		session, ok := byRoot[key]
		if !ok {
			session = model.SessionInfo{
				SessionID: "codex:" + key,
				AgentType: "codex",
				NativeID:  key,
				Status:    "available",
			}
		}
		return session
	}

	// Index entries provide display names and activity timestamps. Entries
	// mapping to the same root (for example a thread forked after compact)
	// collapse into one session. Use the latest real thread name across the
	// lineage; the root index entry may be absent after a fork or compact.
	for _, entry := range entries {
		if entry.ID == "" {
			continue
		}
		key := lineage.rootOf(entry.ID)
		session := sessionFor(key)
		threadName := strings.TrimSpace(entry.ThreadName)
		if threadName != "" && (session.Name == "" || laterTimeString(nameUpdatedAt[key], entry.UpdatedAt) == entry.UpdatedAt) {
			session.Name = threadName
			nameUpdatedAt[key] = entry.UpdatedAt
		}
		session.UpdatedAt = laterTimeString(session.UpdatedAt, entry.UpdatedAt)
		session.LastActive = session.UpdatedAt
		byRoot[key] = session
	}

	// Rollout metadata covers threads missing from the index and fills in
	// workspace info. Subagent rollouts are internal to a conversation and
	// are not reported as sessions of their own.
	for _, rollout := range metas {
		meta := rollout.meta
		if meta.Payload.ID == "" || meta.isSubagent() {
			continue
		}
		key := lineage.conversationKey(meta)
		session := sessionFor(key)
		if meta.Payload.ID == key && session.Name == "" {
			session.Name = localSessionName(meta.Payload.CWD, key)
		}
		session.UpdatedAt = laterTimeString(
			session.UpdatedAt,
			firstNonEmpty(rollout.activityAt, meta.Payload.Timestamp),
		)
		session.LastActive = laterTimeString(session.LastActive, session.UpdatedAt)
		if meta.Payload.CWD != "" && (session.ProjectID == "" || meta.Payload.ID == key) {
			session.ProjectID = meta.Payload.CWD
			session.WorkspaceRoots = []string{meta.Payload.CWD}
		}
		session.Status = firstNonEmpty(session.Status, "available")
		byRoot[key] = session
	}

	out := make([]model.SessionInfo, 0, len(byRoot))
	for key, session := range byRoot {
		if session.Name == "" {
			session.Name = key
		}
		out = append(out, session)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
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

func readCodexSessionIndex(path string) ([]codexIndexEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var out []codexIndexEntry
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var entry codexIndexEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil || entry.ID == "" {
			continue
		}
		out = append(out, entry)
	}
	return out, scanner.Err()
}

func readCodexRolloutMetas(root string) ([]codexRolloutMeta, error) {
	var out []codexRolloutMeta
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
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
		activityAt, err := readCodexRolloutLatestTimestamp(path)
		if err != nil {
			return err
		}
		rollout := codexRolloutMeta{path: path, activityAt: activityAt, meta: meta}
		if info, err := entry.Info(); err == nil {
			rollout.mtime = info.ModTime()
		}
		out = append(out, rollout)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func readCodexRolloutLatestTimestamp(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}

	const blockSize int64 = 64 * 1024
	offset := info.Size()
	var suffix []byte
	for offset > 0 {
		readSize := min(blockSize, offset)
		offset -= readSize
		chunk := make([]byte, readSize)
		if _, err := file.ReadAt(chunk, offset); err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		data := make([]byte, 0, len(chunk)+len(suffix))
		data = append(data, chunk...)
		data = append(data, suffix...)
		lines := bytes.Split(data, []byte{'\n'})
		firstCompleteLine := 0
		if offset > 0 {
			firstCompleteLine = 1
		}
		for index := len(lines) - 1; index >= firstCompleteLine; index-- {
			if timestamp := codexRolloutLineTimestamp(lines[index]); timestamp != "" {
				return timestamp, nil
			}
		}
		suffix = append(suffix[:0], lines[0]...)
	}
	return "", nil
}

func codexRolloutLineTimestamp(line []byte) string {
	if len(bytes.TrimSpace(line)) == 0 {
		return ""
	}
	var envelope struct {
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &envelope); err == nil && envelope.Timestamp != "" {
		return envelope.Timestamp
	}
	var meta codexSessionMetaLine
	if err := json.Unmarshal(line, &meta); err == nil && meta.Type == "session_meta" {
		return meta.Payload.Timestamp
	}
	return ""
}

// laterTimeString returns the later of two RFC 3339 timestamps, tolerating
// empty values and unparseable input.
func laterTimeString(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	parsedA, errA := time.Parse(time.RFC3339Nano, a)
	parsedB, errB := time.Parse(time.RFC3339Nano, b)
	if errA != nil || errB != nil {
		if b > a {
			return b
		}
		return a
	}
	if parsedB.After(parsedA) {
		return b
	}
	return a
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
	dirs := []string{"sessions", "archived_sessions"}
	if found := findCodexLineageRollout(root, dirs, nativeID); found != "" {
		return found, nil
	}
	for _, dir := range dirs {
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

// findCodexLineageRollout resolves nativeID to the most recently modified
// rollout in its conversation lineage, so sessions reported by conversation
// root still render content written after a compact/resume fork.
func findCodexLineageRollout(root string, dirs []string, nativeID string) string {
	var metas []codexRolloutMeta
	for _, dir := range dirs {
		found, err := readCodexRolloutMetas(filepath.Join(root, dir))
		if err == nil {
			metas = append(metas, found...)
		}
	}
	if len(metas) == 0 {
		return ""
	}
	// A direct subagent rollout id always renders its own file.
	for _, rollout := range metas {
		if rollout.meta.Payload.ID == nativeID && rollout.meta.isSubagent() {
			return rollout.path
		}
	}
	lineage := newCodexLineage(metas)
	key := lineage.rootOf(nativeID)
	best := ""
	var bestTime time.Time
	for _, rollout := range metas {
		if rollout.meta.isSubagent() {
			continue
		}
		if lineage.conversationKey(rollout.meta) != key {
			continue
		}
		if best == "" || rollout.mtime.After(bestTime) {
			best = rollout.path
			bestTime = rollout.mtime
		}
	}
	return best
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
