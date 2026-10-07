package harnessauth

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
)

func (m *Manager) commandFor(harness string) string {
	if m.command != "" {
		return m.command
	}
	return harness
}

// The native status command can print a masked API key. Never return its output.
func (m *Manager) codexStatus(ctx context.Context) (control.HarnessAuthView, error) {
	cmd := exec.CommandContext(ctx, m.commandFor("codex"), "login", "status")
	prepareLoginCommand(cmd)
	cmd.WaitDelay = time.Second
	output := &limitedOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	err := cmd.Run()
	_ = stopLoginCommand(cmd)
	text := strings.TrimSpace(string(output.data))
	loggedIn, method := false, ""
	switch {
	case err == nil && strings.Contains(text, "Logged in using ChatGPT"):
		loggedIn, method = true, "chatgpt"
	case err == nil && strings.Contains(text, "Logged in using an API key"):
		loggedIn, method = true, "api_key"
	case err == nil && (strings.Contains(text, "Logged in using access token") || strings.Contains(text, "Logged in using personal access token")):
		loggedIn, method = true, "access_token"
	case err == nil && strings.Contains(text, "Logged in using workload identity"):
		loggedIn, method = true, "workload_identity"
	default:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || text != "Not logged in" {
			return control.HarnessAuthView{}, authError("status_failed", "could not determine Codex authentication status")
		}
	}
	state := "logged_out"
	if loggedIn {
		state = "logged_in"
	}
	return control.HarnessAuthView{Harness: "codex", State: state, LoggedIn: &loggedIn, AuthMethod: method}, nil
}

var ansiCSI = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
var deviceCode = regexp.MustCompile(`\b([A-Z0-9]{4,6}-[A-Z0-9]{4,6})[ \t\r\n]`)

func (w *loginOutput) parseCodexDevice() {
	text := ansiCSI.ReplaceAllString(w.tail, "")
	const address = "https://auth.openai.com/codex/device"
	for _, loc := range urlPattern.FindAllStringIndex(text, -1) {
		if loc[1] == len(text) || text[loc[0]:loc[1]] != address {
			continue
		}
		match := deviceCode.FindStringSubmatch(text[loc[1]:])
		if len(match) != 2 {
			continue
		}
		w.session.view.AuthorizationURL = address
		w.session.view.UserCode = match[1]
		w.session.view.State = "awaiting_browser"
		w.tail = ""
		return
	}
}
