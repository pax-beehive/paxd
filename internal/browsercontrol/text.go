package browsercontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// Text uses the same owner and sequence fence as interactive RFB input. The
// fixed container helper accepts JSON on stdin; no text is put in argv or logs.
func (c *Client) dockerText(ctx context.Context, source string, payload json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Action struct {
			VNCID string `json:"vncID"`
			Seq   uint64 `json:"seq"`
			Text  string `json:"text"`
		} `json:"action"`
	}
	if !utf8.Valid(payload) || json.Unmarshal(payload, &request) != nil || !utf8.ValidString(request.Action.Text) ||
		utf8.RuneCountInString(request.Action.Text) == 0 || utf8.RuneCountInString(request.Action.Text) > 2000 || strings.ContainsRune(request.Action.Text, 0) {
		return nil, errors.New("invalid Docker text input")
	}
	c.mu.Lock()
	session := c.vncSessions[request.Action.VNCID]
	c.mu.Unlock()
	if session == nil || session.source != source {
		return nil, errors.New("active Docker control connection required")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed || session.stage < 2 || time.Since(session.created) > 30*time.Minute {
		return nil, errors.New("Docker control connection unavailable")
	}
	if request.Action.Seq != session.seq {
		session.close()
		return nil, errors.New("VNC sequence mismatch; reconnect")
	}
	session.seq++
	session.timer.Reset(30 * time.Second)
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	input := c.dockerTextInput
	if input == nil {
		input = pasteDockerText
	}
	if err := input(ctx, request.Action.Text); err != nil {
		session.close()
		return nil, errors.New("Docker text delivery failed; update the browser runtime if needed and inspect before retrying")
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func pasteDockerText(ctx context.Context, text string) error {
	request, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		return err
	}
	docker, err := dockerExecutable()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, docker, "exec", "-i", "agent-browser", "/usr/bin/flock", "-n", "/tmp/agent-browser-desktop-text.lock", "node", "/opt/agent-browser/desktop-text.cjs")
	cmd.Stdin = bytes.NewReader(request)
	// The helper never prints text; do not retain container output in the daemon.
	return cmd.Run()
}

func dockerExecutable() (string, error) {
	if path, err := exec.LookPath("docker"); err == nil {
		return path, nil
	}
	// LaunchAgents do not necessarily inherit the user's interactive shell PATH.
	if runtime.GOOS == "darwin" {
		for _, path := range []string{"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker"} {
			if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				return path, nil
			}
		}
	}
	return "", errors.New("Docker executable unavailable")
}
