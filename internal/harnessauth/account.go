package harnessauth

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pax-beehive/paxd/internal/control"
)

func publicAccount(email, name, organization, plan string) *control.HarnessAuthAccount {
	account := &control.HarnessAuthAccount{
		Email:        accountField(email, 254),
		DisplayName:  accountField(name, 256),
		Organization: accountField(organization, 256),
		Plan:         accountField(plan, 128),
	}
	if *account == (control.HarnessAuthAccount{}) {
		return nil
	}
	return account
}

func accountField(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit || !utf8.ValidString(value) || strings.ContainsFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
	}) {
		return ""
	}
	return value
}

func nativeAccountString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// Account identity is best-effort and never changes the native login result.
// Use the public CLI protocol rather than reading tokens or credential files.
func (m *Manager) codexAccount(ctx context.Context) *control.HarnessAuthAccount {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.commandFor("codex"), "app-server")
	prepareLoginCommand(cmd)
	cmd.WaitDelay = time.Second
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	if cmd.Start() != nil {
		return nil
	}
	defer func() {
		_ = stdin.Close()
		_ = stopLoginCommand(cmd)
		_ = cmd.Wait()
	}()
	return readCodexAccount(stdin, stdout)
}

func readCodexAccount(stdin io.Writer, stdout io.Reader) *control.HarnessAuthAccount {
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 64*1024)
	budget := 64 * 1024
	if encoder.Encode(map[string]any{
		"id": 1, "method": "initialize", "params": map[string]any{
			"clientInfo": map[string]string{"name": "paxd", "version": "1"},
		},
	}) != nil {
		return nil
	}
	if _, ok := accountResponse(scanner, 1, &budget); !ok {
		return nil
	}
	if encoder.Encode(map[string]any{"method": "initialized"}) != nil {
		return nil
	}
	if encoder.Encode(map[string]any{"id": 2, "method": "account/read", "params": map[string]bool{"refreshToken": false}}) != nil {
		return nil
	}
	result, ok := accountResponse(scanner, 2, &budget)
	if !ok {
		return nil
	}
	var response struct {
		Account *struct {
			Type  string `json:"type"`
			Email string `json:"email"`
			Plan  string `json:"planType"`
		} `json:"account"`
	}
	if json.Unmarshal(result, &response) != nil || response.Account == nil || response.Account.Type != "chatgpt" {
		return nil
	}
	return publicAccount(response.Account.Email, "", "", response.Account.Plan)
}

func accountResponse(scanner *bufio.Scanner, id int, budget *int) (json.RawMessage, bool) {
	for scanner.Scan() {
		*budget -= len(scanner.Bytes()) + 1
		if *budget < 0 {
			return nil, false
		}
		var frame struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			return nil, false
		}
		if string(frame.ID) != strconv.Itoa(id) {
			continue
		}
		return frame.Result, len(frame.Result) > 0 && (len(frame.Error) == 0 || string(frame.Error) == "null")
	}
	return nil, false
}
