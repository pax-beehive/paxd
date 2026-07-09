package acpclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
		Timeout: time.Second,
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
