package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestAgentsSetupDryRunPrintsInstallCommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runAgentSetupCommands(context.Background(), &stdout, &stderr, "test", [][]string{
		{"npm", "install", "-g", "@zed-industries/codex-acp"},
		{"npm", "install", "-g", "pi-acp", "@earendil-works/pi-coding-agent"},
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
}
