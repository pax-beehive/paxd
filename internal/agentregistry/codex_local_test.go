package agentregistry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCodexLineageFixture builds a CODEX_HOME where thread aaa was forked
// into bbb (compact/resume lineage) and has a guardian subagent rollout ddd.
func writeCodexLineageFixture(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	writeRollout := func(day, name string, lines []string, mtime time.Time) {
		t.Helper()
		dir := filepath.Join(home, "sessions", "2026", "07", day)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		path := filepath.Join(dir, name)
		content := ""
		for _, line := range lines {
			content += line + "\n"
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatalf("Chtimes(%s) error = %v", name, err)
		}
	}
	message := func(ts, text string) string {
		return `{"timestamp":"` + ts + `","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}}`
	}

	writeRollout("18", "rollout-2026-07-18T01-00-00-aaa.jsonl", []string{
		`{"type":"session_meta","payload":{"id":"aaa","session_id":"aaa","timestamp":"2026-07-18T01:00:00Z","cwd":"/tmp/project","source":"vscode"}}`,
		message("2026-07-18T01:00:01Z", "old turn"),
	}, time.Date(2026, 7, 18, 1, 0, 0, 0, time.UTC))
	writeRollout("19", "rollout-2026-07-19T01-00-00-bbb.jsonl", []string{
		`{"type":"session_meta","payload":{"id":"bbb","session_id":"bbb","forked_from_id":"aaa","timestamp":"2026-07-19T01:00:00Z","cwd":"/tmp/project","source":"vscode"}}`,
		message("2026-07-19T01:00:01Z", "new turn after compact"),
	}, time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC))
	writeRollout("19", "rollout-2026-07-19T02-00-00-ddd.jsonl", []string{
		`{"type":"session_meta","payload":{"id":"ddd","session_id":"aaa","forked_from_id":"aaa","timestamp":"2026-07-19T02:00:00Z","cwd":"/tmp/project","source":{"subagent":{"other":"guardian"}},"thread_source":"subagent"}}`,
		message("2026-07-19T02:00:01Z", "guardian review"),
	}, time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC))

	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(
		`{"id":"aaa","thread_name":"original thread","updated_at":"2026-07-18T02:00:00Z"}`+"\n"+
			`{"id":"bbb","thread_name":"forked thread","updated_at":"2026-07-19T03:00:00Z"}`+"\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile(index) error = %v", err)
	}
	return home
}

func TestListCodexLocalSessionsCollapsesForkLineage(t *testing.T) {
	writeCodexLineageFixture(t)

	sessions, err := listCodexLocalSessions()
	if err != nil {
		t.Fatalf("listCodexLocalSessions() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1: %+v", len(sessions), sessions)
	}
	session := sessions[0]
	if session.SessionID != "codex:aaa" || session.NativeID != "aaa" || session.AgentType != "codex" {
		t.Fatalf("session identity = %+v, want codex:aaa", session)
	}
	if session.Name != "original thread" {
		t.Fatalf("session.Name = %q, want root thread title", session.Name)
	}
	if session.UpdatedAt != "2026-07-19T03:00:00Z" {
		t.Fatalf("session.UpdatedAt = %q, want latest index activity", session.UpdatedAt)
	}
	if session.ProjectID != "/tmp/project" || len(session.WorkspaceRoots) != 1 || session.WorkspaceRoots[0] != "/tmp/project" {
		t.Fatalf("session workspace = %+v, want /tmp/project", session)
	}
	if session.Status != "available" {
		t.Fatalf("session.Status = %q, want available", session.Status)
	}
}

func TestListCodexLocalSessionsMergesResumeRollout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "07", "19")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	write := func(name, meta string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(meta+"\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", name, err)
		}
	}
	write("rollout-2026-07-18T01-00-00-aaa.jsonl",
		`{"type":"session_meta","payload":{"id":"aaa","session_id":"aaa","timestamp":"2026-07-18T01:00:00Z","cwd":"/tmp/project","source":"cli"}}`)
	// A resumed conversation reuses the original session_id without forking.
	write("rollout-2026-07-19T01-00-00-ccc.jsonl",
		`{"type":"session_meta","payload":{"id":"ccc","session_id":"aaa","timestamp":"2026-07-19T01:00:00Z","cwd":"/tmp/project","source":"cli"}}`)

	sessions, err := listCodexLocalSessions()
	if err != nil {
		t.Fatalf("listCodexLocalSessions() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1: %+v", len(sessions), sessions)
	}
	if sessions[0].SessionID != "codex:aaa" || sessions[0].NativeID != "aaa" {
		t.Fatalf("session = %+v, want codex:aaa", sessions[0])
	}
	if sessions[0].Name != "project (aaa)" {
		t.Fatalf("session.Name = %q, want cwd-derived name, not the raw source %q", sessions[0].Name, "cli")
	}
}

func TestCodexLocalElementsReadsLatestLineageRollout(t *testing.T) {
	writeCodexLineageFixture(t)

	elements, err := CodexLocalElements("aaa")
	if err != nil {
		t.Fatalf("CodexLocalElements() error = %v", err)
	}
	if len(elements) != 1 || elements[0].ContentText != "new turn after compact" {
		t.Fatalf("elements = %+v, want content from the latest fork rollout", elements)
	}
}

func TestCodexLocalElementsReadsSubagentRolloutItself(t *testing.T) {
	writeCodexLineageFixture(t)

	elements, err := CodexLocalElements("ddd")
	if err != nil {
		t.Fatalf("CodexLocalElements() error = %v", err)
	}
	if len(elements) != 1 || elements[0].ContentText != "guardian review" {
		t.Fatalf("elements = %+v, want the subagent rollout itself", elements)
	}
}
