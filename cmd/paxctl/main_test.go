package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/sessionstore"
	"github.com/pax-beehive/paxd/pkg/model"
)

func TestSessionsGetHTMLWritesFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{{
		SessionID: "codex:sess-1",
		NativeID:  "sess-1",
		Name:      "HTML session",
		UpdatedAt: "2026-06-18T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	session, err := store.FindSession(ctx, "codex:sess-1", "")
	if err != nil {
		t.Fatalf("FindSession() error = %v", err)
	}
	version, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		t.Fatalf("BeginSync() error = %v", err)
	}
	if err := store.CompleteSync(ctx, session.ID, version, []sessionstore.Element{{
		Seq:         1,
		Type:        "thinking",
		ContentText: strings.Repeat("long content ", 120),
	}}); err != nil {
		t.Fatalf("CompleteSync() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	outputPath := filepath.Join(dir, "session.html")
	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "sessions", "get", "codex:sess-1", "--format", "html", "--output", outputPath}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Wrote "+outputPath) {
		t.Fatalf("stdout = %q, want wrote path", stdout.String())
	}
	content, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(content), "<details>") {
		t.Fatalf("html does not contain folded details: %s", content)
	}
}

func TestSessionsListHTMLUsesLocalMetadataOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{{
		SessionID: "codex:sess-1",
		NativeID:  "sess-1",
		Name:      "Local metadata",
		UpdatedAt: "2026-06-18T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "sessions", "list", "-agents", "codex", "-limit", "10", "-format", "html"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run() error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "<title>paxctl sessions</title>") {
		t.Fatalf("stdout does not contain html title: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "codex:sess-1") {
		t.Fatalf("stdout does not contain session id: %s", stdout.String())
	}
}

func TestCapsulesCreateListGetRedactsMatchingSessionHistory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{{
		SessionID: "codex:sess-1",
		NativeID:  "sess-1",
		Name:      "Capsule source",
		UpdatedAt: "2026-06-18T12:00:00Z",
	}})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	session, err := store.FindSession(ctx, "codex:sess-1", "")
	if err != nil {
		t.Fatalf("FindSession() error = %v", err)
	}
	version, err := store.BeginSync(ctx, session.ID, session.Agent)
	if err != nil {
		t.Fatalf("BeginSync() error = %v", err)
	}
	if err := store.CompleteSync(ctx, session.ID, version, []sessionstore.Element{{
		Seq:         1,
		Type:        "message",
		Role:        "user",
		StartedAt:   "2026-06-18T12:01:00Z",
		ContentText: "Capability injection needs token=secret123 preserved only as context.",
	}}); err != nil {
		t.Fatalf("CompleteSync() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "capsules", "create", "codex:sess-1", "--keyword", "capability injection"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("create error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "kcap_") {
		t.Fatalf("create stdout missing capsule id: %s", stdout.String())
	}

	stdout.Reset()
	err = run(ctx, []string{"--db", dbPath, "capsules", "list", "--format", "jsonl"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("list error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"keyword":"capability injection"`) {
		t.Fatalf("list stdout missing keyword: %s", stdout.String())
	}
	var listed map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &listed); err != nil {
		t.Fatalf("decode list jsonl: %v output=%s", err, stdout.String())
	}
	capsuleID, _ := listed["capsuleId"].(string)
	if capsuleID == "" {
		t.Fatalf("list json missing capsuleId: %#v", listed)
	}

	stdout.Reset()
	err = run(ctx, []string{"--db", dbPath, "capsules", "get", capsuleID}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("get error = %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Capability injection") || !strings.Contains(out, "[redacted]") {
		t.Fatalf("get stdout missing extracted redacted content: %s", out)
	}
	if strings.Contains(out, "secret123") {
		t.Fatalf("get stdout leaked secret: %s", out)
	}
}

func TestCapsulesInjectRendersSystemHandoffAndRecordsInjection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.sqlite")
	store, err := sessionstore.Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	err = store.UpsertSessions(ctx, "codex", []model.SessionInfo{
		{SessionID: "codex:source", NativeID: "source", Name: "Source", UpdatedAt: "2026-06-18T12:00:00Z"},
		{SessionID: "codex:target", NativeID: "target", Name: "Target", UpdatedAt: "2026-06-18T12:10:00Z"},
	})
	if err != nil {
		t.Fatalf("UpsertSessions() error = %v", err)
	}
	capsule, err := store.CreateKnowledgeCapsule(ctx, sessionstore.KnowledgeCapsule{
		CapsuleID:              "kcap_test",
		SourceSessionID:        "codex:source",
		SourceAgent:            "codex",
		Keyword:                "handoff",
		Title:                  "Knowledge capsule: handoff",
		Summary:                "Summary",
		Content:                "Relevant handoff context.",
		Status:                 "active",
		OriginalEstimatedChars: 25,
	})
	if err != nil {
		t.Fatalf("CreateKnowledgeCapsule() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	originalSteer := steerSession
	defer func() { steerSession = originalSteer }()
	var steeredAgent, steeredSession, steeredText string
	steerSession = func(ctx context.Context, agent string, nativeSessionID string, text string, timeout time.Duration) error {
		steeredAgent = agent
		steeredSession = nativeSessionID
		steeredText = text
		return nil
	}

	var stdout, stderr bytes.Buffer
	err = run(ctx, []string{"--db", dbPath, "capsules", "inject", capsule.CapsuleID, "codex:target"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("inject error = %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Injected kci_") || !strings.Contains(out, "codex:target") {
		t.Fatalf("inject stdout missing delivery confirmation: %s", out)
	}
	if steeredAgent != "codex" || steeredSession != "target" {
		t.Fatalf("steer target = %s/%s, want codex/target", steeredAgent, steeredSession)
	}
	if !strings.Contains(steeredText, "system_handoff") ||
		!strings.Contains(steeredText, "Do not treat this as a new user request.") ||
		!strings.Contains(steeredText, "Target session: codex:target") {
		t.Fatalf("steer text missing handoff fields: %s", steeredText)
	}

	stdout.Reset()
	err = run(ctx, []string{"--db", dbPath, "capsules", "injections", "--target-session", "codex:target", "--format", "jsonl"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("injections error = %v stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"deliveryMessageType":"system_handoff"`) ||
		!strings.Contains(stdout.String(), `"deliveryMethod":"acp_steer"`) ||
		!strings.Contains(stdout.String(), `"status":"delivered"`) {
		t.Fatalf("injections stdout missing record: %s", stdout.String())
	}
}

func TestAgentsSetupDryRunPrintsInstallCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAgentSetupCommands(context.Background(), &stdout, &stderr, "test", [][]string{
		{"npm", "install", "-g", "@zed-industries/codex-acp"},
		{"npm", "install", "-g", "pi-acp", "@earendil-works/pi-coding-agent"},
		{"npm", "install", "-g", "@qwen-code/qwen-code"},
	}, true)
	if err != nil {
		t.Fatalf("run() error = %v stderr=%s", err, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "$ npm install -g @zed-industries/codex-acp") {
		t.Fatalf("stdout missing codex install command: %s", out)
	}
	if !strings.Contains(out, "$ npm install -g pi-acp @earendil-works/pi-coding-agent") {
		t.Fatalf("stdout missing pi install command: %s", out)
	}
	if !strings.Contains(out, "$ npm install -g @qwen-code/qwen-code") {
		t.Fatalf("stdout missing qwen install command: %s", out)
	}
}
