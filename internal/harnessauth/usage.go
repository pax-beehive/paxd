package harnessauth

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
)

const (
	// The Claude CLI caches plan usage for about a minute, so refreshing sooner
	// cannot return newer data.
	usageFreshTTL      = time.Minute
	usageRetryTTL      = 15 * time.Second
	claudeUsageTimeout = 10 * time.Second
	codexUsageTimeout  = 5 * time.Second
	usageExitGrace     = 3 * time.Second
	usageMaxWindows    = 12
	claudeUsageLine    = 1 << 20
	claudeUsageBudget  = 4 << 20
	codexUsageBudget   = 128 << 10
	usageWeekMinutes   = 7 * 24 * 60
)

// claudeUsageArgs starts a prompt-less SDK control session. No user message is
// sent, so no model request is made. MCP servers and hooks are disabled so user
// or project configuration cannot run code or open connections for a read.
var claudeUsageArgs = []string{
	"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
	"--strict-mcp-config", "--settings", `{"disableAllHooks":true}`,
}

type UsageOptions struct {
	// Command overrides the native executable for tests.
	Command string
	// WorkDir must be a private, paxd-owned directory: native CLIs load project
	// settings, hooks, and instructions from their working directory.
	WorkDir string
	Now     func() time.Time
}

// UsageReader reads plan usage through each harness CLI's public protocol and
// caches the bounded result per harness. Like Manager it never reads credential
// files; the native CLI fetches usage with its own credentials.
type UsageReader struct {
	ctx     context.Context
	cancel  context.CancelFunc
	command string
	workDir string
	now     func() time.Time
	wg      sync.WaitGroup
	// Bounds for one native read; fixed outside tests.
	claudeTimeout, codexTimeout, exitGrace time.Duration

	mu      sync.Mutex
	closed  bool
	entries map[string]*usageEntry
}

type usageEntry struct {
	view    control.HarnessUsageView
	expires time.Time
	done    chan struct{}
}

func NewUsageReader(ctx context.Context, opts UsageOptions) *UsageReader {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	ctx, cancel := context.WithCancel(ctx)
	return &UsageReader{
		ctx: ctx, cancel: cancel, command: opts.Command, workDir: opts.WorkDir, now: opts.Now,
		claudeTimeout: claudeUsageTimeout, codexTimeout: codexUsageTimeout, exitGrace: usageExitGrace,
		entries: map[string]*usageEntry{},
	}
}

func (r *UsageReader) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

// Usage returns the cached view or joins the single in-flight native read. The
// read is detached from the caller so one cancelled request cannot abort a read
// that other callers are waiting for.
func (r *UsageReader) Usage(ctx context.Context, q control.HarnessUsageStatusQuery) (control.HarnessUsageView, error) {
	if err := q.Validate(); err != nil {
		return control.HarnessUsageView{}, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return control.HarnessUsageView{}, authError("unavailable", "usage reader stopped")
	}
	entry := r.entries[q.Harness]
	if entry == nil {
		entry = &usageEntry{}
		r.entries[q.Harness] = entry
	}
	if entry.done == nil && r.now().Before(entry.expires) {
		view := cloneUsageView(entry.view)
		r.mu.Unlock()
		return view, nil
	}
	if entry.done == nil {
		entry.done = make(chan struct{})
		r.wg.Add(1)
		go r.refresh(q.Harness, entry, entry.done)
	}
	done := entry.done
	r.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return control.HarnessUsageView{}, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneUsageView(entry.view), nil
}

func (r *UsageReader) refresh(harness string, entry *usageEntry, done chan struct{}) {
	defer r.wg.Done()
	view := r.read(harness)
	ttl := usageFreshTTL
	if view.State == control.HarnessUsageUnavailable {
		ttl = usageRetryTTL
	}
	r.mu.Lock()
	entry.view, entry.expires, entry.done = view, r.now().Add(ttl), nil
	r.mu.Unlock()
	close(done)
}

func (r *UsageReader) read(harness string) control.HarnessUsageView {
	var view control.HarnessUsageView
	switch {
	case r.workDir == "" || os.MkdirAll(r.workDir, 0o700) != nil:
		view = unavailableUsage("read_failed")
	case harness == "codex":
		view = r.run(harness, r.codexTimeout, []string{"app-server"}, readCodexUsage)
	default:
		view = r.run(harness, r.claudeTimeout, claudeUsageArgs, readClaudeUsage)
	}
	view.Harness = harness
	view.ObservedAt = r.now().UTC().Format(time.RFC3339)
	return view
}

// run drives one native JSON session over stdin/stdout. After the read, the
// session ends in the background: stdin closes, the CLI may exit on its own so
// it can finish its own cache writes, and the process group is then stopped.
func (r *UsageReader) run(
	harness string, timeout time.Duration, args []string,
	read func(io.Writer, io.Reader) control.HarnessUsageView,
) control.HarnessUsageView {
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	command := harness
	if r.command != "" {
		command = r.command
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = r.workDir
	prepareLoginCommand(cmd)
	cmd.WaitDelay = time.Second
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return unavailableUsage("read_failed")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		cancel()
		return unavailableUsage("read_failed")
	}
	if cmd.Start() != nil {
		cancel()
		return unavailableUsage("read_failed")
	}
	view := read(stdin, stdout)
	if view.State == control.HarnessUsageUnavailable && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		view.ErrorCode = "timeout"
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer cancel()
		_ = stdin.Close()
		waitForUsageExit(cmd, r.exitGrace)
	}()
	return view
}

func waitForUsageExit(cmd *exec.Cmd, grace time.Duration) {
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-exited:
	case <-timer.C:
		_ = stopLoginCommand(cmd)
		<-exited
	}
	// Descendants can outlive the CLI's own exit.
	_ = stopLoginCommand(cmd)
}

func readClaudeUsage(stdin io.Writer, stdout io.Reader) control.HarnessUsageView {
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), claudeUsageLine)
	budget := claudeUsageBudget
	if encoder.Encode(claudeControlRequest("pax-usage-init", map[string]any{"subtype": "initialize"})) != nil {
		return unavailableUsage("read_failed")
	}
	if _, code := claudeControlResponse(scanner, "pax-usage-init", &budget); code != "" {
		return unavailableUsage(code)
	}
	request := map[string]any{"subtype": "get_usage", "skip_behaviors": true}
	if encoder.Encode(claudeControlRequest("pax-usage-read", request)) != nil {
		return unavailableUsage("read_failed")
	}
	body, code := claudeControlResponse(scanner, "pax-usage-read", &budget)
	if code != "" {
		return unavailableUsage(code)
	}
	return claudeUsageView(body)
}

func claudeControlRequest(id string, request map[string]any) map[string]any {
	return map[string]any{"type": "control_request", "request_id": id, "request": request}
}

// claudeControlResponse returns the matching success payload or an error code.
// Native error text is only classified, never returned.
func claudeControlResponse(scanner *bufio.Scanner, id string, budget *int) (json.RawMessage, string) {
	for scanner.Scan() {
		*budget -= len(scanner.Bytes()) + 1
		if *budget < 0 {
			return nil, "read_failed"
		}
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
				Error     json.RawMessage `json:"error"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.Type != "control_response" ||
			frame.Response.RequestID != id {
			continue
		}
		if frame.Response.Subtype == "success" {
			return frame.Response.Response, ""
		}
		if strings.HasPrefix(nativeAccountString(frame.Response.Error), "Unsupported control request subtype") {
			return nil, "cli_unsupported"
		}
		return nil, "read_failed"
	}
	return nil, "read_failed"
}

type nativeUsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// claudeUsageView maps only the typed SDK fields of the get_usage response.
// Each window decodes separately so one malformed entry cannot hide the rest.
func claudeUsageView(body json.RawMessage) control.HarnessUsageView {
	var response struct {
		SubscriptionType    json.RawMessage            `json:"subscription_type"`
		RateLimitsAvailable json.RawMessage            `json:"rate_limits_available"`
		RateLimits          map[string]json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(body, &response) != nil {
		return unavailableUsage("read_failed")
	}
	view := control.HarnessUsageView{
		State: control.HarnessUsageUnsupported,
		Plan:  accountField(nativeAccountString(response.SubscriptionType), 128),
	}
	var available bool
	if json.Unmarshal(response.RateLimitsAvailable, &available) != nil || !available || response.RateLimits == nil {
		return view
	}
	view.State = control.HarnessUsageAvailable
	var windows usageWindows
	for _, named := range []struct {
		key, scope string
		minutes    int
	}{
		{"five_hour", "", 5 * 60},
		{"seven_day", "", usageWeekMinutes},
		{"seven_day_opus", "Opus", usageWeekMinutes},
		{"seven_day_sonnet", "Sonnet", usageWeekMinutes},
	} {
		var window nativeUsageWindow
		if json.Unmarshal(response.RateLimits[named.key], &window) == nil {
			windows.add(named.key, named.minutes, named.scope, window.Utilization, usageTime(window.ResetsAt))
		}
	}
	var scoped []json.RawMessage
	if json.Unmarshal(response.RateLimits["model_scoped"], &scoped) == nil {
		for _, raw := range scoped {
			var window struct {
				DisplayName string `json:"display_name"`
				nativeUsageWindow
			}
			if json.Unmarshal(raw, &window) != nil {
				continue
			}
			scope := accountField(window.DisplayName, 64)
			if suffix := usageScopeID(scope); suffix != "" {
				windows.add("seven_day_"+suffix, usageWeekMinutes, scope, window.Utilization, usageTime(window.ResetsAt))
			}
		}
	}
	view.Windows = windows.list
	var extra struct {
		IsEnabled *bool `json:"is_enabled"`
	}
	if json.Unmarshal(response.RateLimits["extra_usage"], &extra) == nil && extra.IsEnabled != nil {
		view.ExtraUsage = &control.HarnessUsageExtra{Enabled: *extra.IsEnabled}
	}
	return view
}

func readCodexUsage(stdin io.Writer, stdout io.Reader) control.HarnessUsageView {
	encoder := json.NewEncoder(stdin)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024), 64*1024)
	budget := codexUsageBudget
	if encoder.Encode(map[string]any{
		"id": 1, "method": "initialize", "params": map[string]any{
			"clientInfo": map[string]string{"name": "paxd", "version": "1"},
		},
	}) != nil {
		return unavailableUsage("read_failed")
	}
	if _, ok := accountResponse(scanner, 1, &budget); !ok {
		return unavailableUsage("read_failed")
	}
	if encoder.Encode(map[string]any{"method": "initialized"}) != nil ||
		encoder.Encode(map[string]any{"id": 2, "method": "account/read", "params": map[string]bool{"refreshToken": false}}) != nil {
		return unavailableUsage("read_failed")
	}
	result, ok := accountResponse(scanner, 2, &budget)
	var account struct {
		Account *struct {
			Type string `json:"type"`
			Plan string `json:"planType"`
		} `json:"account"`
	}
	if !ok || json.Unmarshal(result, &account) != nil {
		return unavailableUsage("read_failed")
	}
	// API-key and other non-ChatGPT sign-ins have no plan rate limits.
	if account.Account == nil || account.Account.Type != "chatgpt" {
		return control.HarnessUsageView{State: control.HarnessUsageUnsupported}
	}
	if encoder.Encode(map[string]any{"id": 3, "method": "account/rateLimits/read"}) != nil {
		return unavailableUsage("read_failed")
	}
	result, rpcErr, ok := codexRPC(scanner, 3, &budget)
	switch {
	case !ok:
		return unavailableUsage("read_failed")
	case rpcErr != nil && (rpcErr.Code == -32600 || rpcErr.Code == -32601):
		// Older app-servers reject the method as an unknown request variant.
		return unavailableUsage("cli_unsupported")
	case rpcErr != nil:
		return unavailableUsage("read_failed")
	}
	view := codexUsageView(result)
	if view.Plan == "" {
		view.Plan = accountField(account.Account.Plan, 128)
	}
	return view
}

type codexUsageWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *int     `json:"windowDurationMins"`
	ResetsAt           *int64   `json:"resetsAt"`
}

func codexUsageView(result json.RawMessage) control.HarnessUsageView {
	var response struct {
		RateLimits *struct {
			Primary   json.RawMessage `json:"primary"`
			Secondary json.RawMessage `json:"secondary"`
			PlanType  json.RawMessage `json:"planType"`
		} `json:"rateLimits"`
	}
	if json.Unmarshal(result, &response) != nil || response.RateLimits == nil {
		return unavailableUsage("read_failed")
	}
	view := control.HarnessUsageView{
		State: control.HarnessUsageAvailable,
		Plan:  accountField(nativeAccountString(response.RateLimits.PlanType), 128),
	}
	var windows usageWindows
	for _, named := range []struct {
		id  string
		raw json.RawMessage
	}{{"primary", response.RateLimits.Primary}, {"secondary", response.RateLimits.Secondary}} {
		var window codexUsageWindow
		if json.Unmarshal(named.raw, &window) != nil || window.WindowDurationMins == nil {
			continue
		}
		resets := ""
		if window.ResetsAt != nil && *window.ResetsAt > 0 {
			resets = time.Unix(*window.ResetsAt, 0).UTC().Format(time.RFC3339)
		}
		windows.add(named.id, *window.WindowDurationMins, "", window.UsedPercent, resets)
	}
	view.Windows = windows.list
	return view
}

type usageWindows struct {
	list []control.HarnessUsageWindow
}

func (w *usageWindows) add(id string, minutes int, scope string, used *float64, resetsAt string) {
	if used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) || *used < 0 ||
		minutes < 1 || minutes > 31*24*60 || len(w.list) >= usageMaxWindows {
		return
	}
	for _, existing := range w.list {
		if existing.ID == id {
			return
		}
	}
	w.list = append(w.list, control.HarnessUsageWindow{
		ID: id, WindowMinutes: minutes, Scope: scope, UsedPercent: math.Min(*used, 100), ResetsAt: resetsAt,
	})
}

// usageScopeID derives a window id suffix ([a-z0-9_], at most 48 bytes) from a
// model bucket display name.
func usageScopeID(scope string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(scope) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

func usageTime(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

func unavailableUsage(code string) control.HarnessUsageView {
	return control.HarnessUsageView{State: control.HarnessUsageUnavailable, ErrorCode: code}
}

func cloneUsageView(view control.HarnessUsageView) control.HarnessUsageView {
	view.Windows = append([]control.HarnessUsageWindow(nil), view.Windows...)
	if view.ExtraUsage != nil {
		extra := *view.ExtraUsage
		view.ExtraUsage = &extra
	}
	return view
}
