package acpclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSessionListerTimesOutWhenACPDoesNotRespond(t *testing.T) {
	started := time.Now()
	lister := SessionLister{
		Command: []string{"sh", "-c", "cat >/dev/null"},
		Timeout: 100 * time.Millisecond,
	}
	_, err := lister.List(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("List() took %s, want fast timeout", elapsed)
	}
}

func TestSessionListerDoesNotHangWhenServerLeavesGrandchild(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("requires a posix shell")
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "grandchild-acp.sh")
	// The server answers both RPCs, then idles with a background child that
	// inherits the stdio pipes, mimicking wrapper adapters whose children
	// outlive the adapter process.
	script := `#!/bin/sh
read line
printf '{"jsonrpc":"2.0","id":1,"result":{"authMethods":[]}}\n'
read line
printf '{"jsonrpc":"2.0","id":2,"result":{"sessions":[{"sessionId":"sess-1"}]}}\n'
sleep 300 &
wait
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	started := time.Now()
	lister := SessionLister{
		Command: []string{scriptPath},
		Timeout: 5 * time.Second,
	}
	sessions, err := lister.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "sess-1" {
		t.Fatalf("sessions = %+v, want sess-1", sessions)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("List() took %s, want prompt cleanup despite lingering grandchild", elapsed)
	}
}

func TestSessionListerDoesNotAuthenticateOpenAIBrowserLogin(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-acp.sh")
	script := `#!/bin/sh
read line
printf '{"jsonrpc":"2.0","id":1,"result":{"authMethods":[{"id":"chatgpt","name":"ChatGPT","type":"external"}]}}\n'
read line
case "$line" in
  *'"method":"authenticate"'*)
    printf '{"jsonrpc":"2.0","id":2,"error":{"code":400,"message":"unexpected authenticate"}}\n'
    ;;
  *'"method":"session/list"'*)
    printf '{"jsonrpc":"2.0","id":2,"result":{"sessions":[{"sessionId":"codex:sess-1","nativeId":"sess-1","title":"Local session"}]}}\n'
    ;;
  *)
    printf '{"jsonrpc":"2.0","id":2,"error":{"code":400,"message":"unexpected method"}}\n'
    ;;
esac
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	sessions, err := SessionLister{
		Command: []string{scriptPath},
		Timeout: time.Second,
	}.List(context.Background())

	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionID != "codex:sess-1" {
		t.Fatalf("sessions = %+v, want codex:sess-1", sessions)
	}
}

func TestSessionPrompterSendsSessionPrompt(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "prompt.json")
	scriptPath := filepath.Join(dir, "fake-acp.sh")
	script := `#!/bin/sh
read line
printf '{"jsonrpc":"2.0","id":1,"result":{"authMethods":[]}}\n'
read line
printf '%s\n' "$line" > "$1"
printf '{"jsonrpc":"2.0","id":2,"result":{}}\n'
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	prompter := SessionPrompter{
		Command: []string{scriptPath, logPath},
		Timeout: 5 * time.Second,
	}
	err := prompter.Prompt(context.Background(), "sess-1", "system_handoff\ncontext")
	if err != nil {
		t.Fatalf("Prompt() error = %v", err)
	}
	payload, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	out := string(payload)
	if !strings.Contains(out, `"method":"session/prompt"`) ||
		!strings.Contains(out, `"sessionId":"sess-1"`) ||
		!strings.Contains(out, `system_handoff`) {
		t.Fatalf("prompt payload missing fields: %s", out)
	}
}
