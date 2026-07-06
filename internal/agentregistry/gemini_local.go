package agentregistry

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/pkg/model"
)

const defaultGeminiLocalSessionLimit = 500

type geminiConversation struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Messages    []geminiMessage `json:"messages"`
}

type geminiPatchLine struct {
	Set geminiConversation `json:"$set"`
}

type geminiProjectsIndex struct {
	Projects map[string]string `json:"projects"`
}

type geminiMessage struct {
	ID        string        `json:"id"`
	Timestamp string        `json:"timestamp"`
	Type      string        `json:"type"`
	Content   geminiContent `json:"content"`
	Model     string        `json:"model"`
}

type geminiContent struct {
	Text string
}

func (c *geminiContent) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		c.Text = text
		return nil
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		c.Text = strings.Join(parts, "\n")
		return nil
	}
	c.Text = string(raw)
	return nil
}

func ListGeminiLocalSessions(ctx context.Context, limit int) ([]model.SessionInfo, error) {
	paths, err := geminiSessionPaths(ctx)
	if err != nil {
		if os.IsNotExist(err) {
			return []model.SessionInfo{}, nil
		}
		return nil, fmt.Errorf("list gemini session paths: %w", err)
	}
	sessionsByID := make(map[string]model.SessionInfo, len(paths))
	for _, path := range paths {
		conversation, err := readGeminiConversation(path)
		if err != nil || conversation.SessionID == "" {
			continue
		}
		session := geminiSession(path, conversation)
		sessionsByID[session.SessionID] = session
	}
	sessions := make([]model.SessionInfo, 0, len(sessionsByID))
	for _, session := range sessionsByID {
		sessions = append(sessions, session)
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		leftTime, leftOK := parseGeminiSessionTime(sessions[i].UpdatedAt)
		rightTime, rightOK := parseGeminiSessionTime(sessions[j].UpdatedAt)
		if leftOK && rightOK {
			return leftTime.After(rightTime)
		}
		return sessions[i].UpdatedAt > sessions[j].UpdatedAt
	})
	if limit <= 0 {
		limit = defaultGeminiLocalSessionLimit
	}
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return sessions, nil
}

func geminiSessionPaths(ctx context.Context) ([]string, error) {
	root, err := geminiRoot()
	if err != nil {
		return nil, fmt.Errorf("resolve gemini root: %w", err)
	}
	var paths []string
	err = filepath.WalkDir(filepath.Join(root, "tmp"), func(
		path string,
		entry fs.DirEntry,
		walkErr error,
	) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Base(filepath.Dir(path)) != "chats" {
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".jsonl") {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func readGeminiConversation(path string) (*geminiConversation, error) {
	switch filepath.Ext(path) {
	case ".json":
		return readGeminiJSONConversation(path)
	case ".jsonl":
		return readGeminiJSONLConversation(path)
	default:
		return nil, fmt.Errorf("unsupported gemini session file %s", path)
	}
}

func readGeminiJSONConversation(path string) (*geminiConversation, error) {
	raw, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, err
	}
	var conversation geminiConversation
	if err := json.Unmarshal(raw, &conversation); err != nil {
		return nil, err
	}
	return &conversation, nil
}

func readGeminiJSONLConversation(path string) (*geminiConversation, error) {
	file, err := os.Open(path) // #nosec G304
	if err != nil {
		return nil, err
	}
	defer file.Close()
	conversation := &geminiConversation{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		applyGeminiJSONLLine(conversation, scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return conversation, nil
}

func applyGeminiJSONLLine(conversation *geminiConversation, raw []byte) {
	var patch geminiPatchLine
	if err := json.Unmarshal(raw, &patch); err == nil {
		mergeGeminiConversation(conversation, &patch.Set)
	}
	var metadata geminiConversation
	if err := json.Unmarshal(raw, &metadata); err == nil {
		mergeGeminiMetadata(conversation, &metadata)
	}
	var message geminiMessage
	if err := json.Unmarshal(raw, &message); err == nil && message.Type != "" {
		conversation.Messages = append(conversation.Messages, message)
	}
}

func mergeGeminiConversation(target *geminiConversation, source *geminiConversation) {
	mergeGeminiMetadata(target, source)
	if len(source.Messages) > 0 {
		target.Messages = source.Messages
	}
}

func mergeGeminiMetadata(target *geminiConversation, source *geminiConversation) {
	target.SessionID = firstNonEmpty(source.SessionID, target.SessionID)
	target.ProjectHash = firstNonEmpty(source.ProjectHash, target.ProjectHash)
	target.StartTime = firstNonEmpty(source.StartTime, target.StartTime)
	target.LastUpdated = firstNonEmpty(source.LastUpdated, target.LastUpdated)
}

func geminiSession(path string, conversation *geminiConversation) model.SessionInfo {
	updatedAt := geminiUpdatedAt(conversation)
	projectRoot := firstNonEmpty(
		geminiProjectRoot(path),
		geminiProjectRootForHash(conversation.ProjectHash),
	)
	return model.SessionInfo{
		SessionID: "gemini:" + strings.TrimPrefix(conversation.SessionID, "gemini:"),
		AgentType: "gemini",
		NativeID:  strings.TrimPrefix(conversation.SessionID, "gemini:"),
		Name: firstNonEmpty(
			geminiTitle(conversation),
			geminiProjectTitle(projectRoot),
			conversation.SessionID,
		),
		ProjectID:      projectRoot,
		LastActive:     updatedAt,
		WorkspaceRoots: nonEmptySlice(projectRoot),
		Status:         "available",
		UpdatedAt:      updatedAt,
	}
}

func geminiTitle(conversation *geminiConversation) string {
	for _, message := range conversation.Messages {
		if message.Type != "user" {
			continue
		}
		if title := geminiTitleCandidate(message.Content.Text); title != "" {
			return title
		}
	}
	return ""
}

func geminiUpdatedAt(conversation *geminiConversation) string {
	updatedAt := firstNonEmpty(conversation.LastUpdated, conversation.StartTime)
	for _, message := range conversation.Messages {
		updatedAt = laterGeminiTimestamp(updatedAt, message.Timestamp)
	}
	return updatedAt
}

func laterGeminiTimestamp(current string, candidate string) string {
	if current == "" {
		return candidate
	}
	if candidate == "" {
		return current
	}
	currentTime, currentOK := parseGeminiSessionTime(current)
	candidateTime, candidateOK := parseGeminiSessionTime(candidate)
	if currentOK && candidateOK {
		if candidateTime.After(currentTime) {
			return candidate
		}
		return current
	}
	if candidate > current {
		return candidate
	}
	return current
}

func parseGeminiSessionTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

func geminiProjectRoot(path string) string {
	projectDir := filepath.Dir(filepath.Dir(path))
	raw, err := os.ReadFile(filepath.Join(projectDir, ".project_root")) // #nosec G304
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func geminiProjectRootForHash(projectHash string) string {
	projectHash = strings.TrimSpace(projectHash)
	if projectHash == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(geminiRootNoError(), "projects.json"))
	if err != nil {
		return ""
	}
	var index geminiProjectsIndex
	if err := json.Unmarshal(raw, &index); err != nil {
		return ""
	}
	for projectRoot, projectName := range index.Projects {
		if projectHash == projectName || projectHash == sha256Hex(projectRoot) {
			return projectRoot
		}
	}
	return ""
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func geminiProjectTitle(projectRoot string) string {
	projectRoot = strings.TrimSpace(projectRoot)
	if projectRoot == "" {
		return ""
	}
	return filepath.Base(projectRoot)
}

func geminiTitleCandidate(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || isNoisyGeminiTitle(value) {
		return ""
	}
	if command := xmlTagValue(value, "command-name"); command != "" {
		return geminiTrimOneLine(command, 80)
	}
	return geminiTrimOneLine(value, 80)
}

func isNoisyGeminiTitle(value string) bool {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<local-command-caveat>") ||
		strings.HasPrefix(lower, "<session_context>") ||
		strings.HasPrefix(lower, "system_handoff") ||
		strings.HasPrefix(lower, "<environment_context>") ||
		strings.HasPrefix(trimmed, "# AGENTS.md instructions for ") ||
		strings.HasPrefix(trimmed, "AGENTS.md instructions for ") ||
		strings.HasPrefix(trimmed, "<INSTRUCTIONS>")
}

func xmlTagValue(value string, tag string) string {
	start := "<" + tag + ">"
	end := "</" + tag + ">"
	startIndex := strings.Index(value, start)
	if startIndex < 0 {
		return ""
	}
	contentStart := startIndex + len(start)
	contentEnd := strings.Index(value[contentStart:], end)
	if contentEnd < 0 {
		return ""
	}
	return strings.TrimSpace(value[contentStart : contentStart+contentEnd])
}

func geminiTrimOneLine(value string, limit int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func geminiRoot() (string, error) {
	if value := os.Getenv("GEMINI_HOME"); value != "" {
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini"), nil
}

func geminiRootNoError() string {
	root, err := geminiRoot()
	if err != nil {
		return ""
	}
	return root
}
