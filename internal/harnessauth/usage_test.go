package harnessauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type usageClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *usageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *usageClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func usageReader(t *testing.T, script string, now func() time.Time) *UsageReader {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "harness")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nprintf x >> \"$0.calls\"\n"+script), 0o700))
	r := NewUsageReader(t.Context(), UsageOptions{Command: path, WorkDir: filepath.Join(dir, "work"), Now: now})
	r.claudeTimeout, r.codexTimeout, r.exitGrace = 2*time.Second, 2*time.Second, 500*time.Millisecond
	t.Cleanup(r.Close)
	return r
}

func usageCalls(t *testing.T, r *UsageReader) int {
	t.Helper()
	data, err := os.ReadFile(r.command + ".calls")
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return len(data)
}

// claudeUsageScript emulates the CLI's stream-json control protocol: it answers
// initialize, then get_usage with body, and exits on its own once stdin closes.
func claudeUsageScript(body string) string {
	return `printf '%s\n' "$*" > "$0.args"
pwd -P > "$0.cwd"
IFS= read -r init
case "$init" in *'"subtype":"initialize"'*) ;; *) exit 3 ;; esac
printf '%s\n' '{"type":"system","subtype":"status"}'
printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"pax-usage-init","response":{"account":{"email":"must-not-return@example.test"}}}}'
IFS= read -r usage
case "$usage" in *'"skip_behaviors":true,"subtype":"get_usage"'*) ;; *) exit 4 ;; esac
[ -f "$0.hold" ] && while [ ! -f "$0.release" ]; do sleep 0.02; done
printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"pax-usage-read","response":` + body + `}}'
cat >/dev/null
: > "$0.exited"
`
}

const claudeUsageBody = `{"session":{"total_cost_usd":0},"subscription_type":"max","rate_limits_available":true,` +
	`"rate_limits":{"five_hour":null,"seven_day":{"utilization":3,"resets_at":"2026-11-16T08:59:59.542873+00:00","limit_dollars":null},` +
	`"seven_day_opus":null,"seven_day_sonnet":{"utilization":12.5,"resets_at":"2026-11-16T09:00:00+00:00"},` +
	`"tangelo":{"utilization":99,"note":"must-not-return"},` +
	`"extra_usage":{"is_enabled":false,"monthly_limit":null,"disabled_reason":"must-not-return"},` +
	`"limits":[{"kind":"weekly_all","percent":3}],` +
	`"model_scoped":[{"display_name":"Fable","utilization":0,"resets_at":"2026-11-16T09:00:00+00:00"},` +
	`{"display_name":"Sonnet","utilization":50,"resets_at":"2026-11-16T09:00:00+00:00"}]},` +
	`"behaviors":{"day":"must-not-return"}}`

var claudeQuery = control.HarnessUsageStatusQuery{Harness: "claude"}

func TestClaudeUsageMapsOnlyTypedWindowsFromAPromptlessSession(t *testing.T) {
	clock := &usageClock{now: time.Date(2026, 11, 12, 17, 31, 51, 0, time.UTC)}
	r := usageReader(t, claudeUsageScript(claudeUsageBody), clock.Now)
	view, err := r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)

	assert.Equal(t, control.HarnessUsageView{
		Harness: "claude", State: control.HarnessUsageAvailable, ObservedAt: "2026-11-12T17:31:51Z", Plan: "max",
		Windows: []control.HarnessUsageWindow{
			{ID: "seven_day", WindowMinutes: 10080, UsedPercent: 3, ResetsAt: "2026-11-16T08:59:59Z"},
			{ID: "seven_day_sonnet", WindowMinutes: 10080, Scope: "Sonnet", UsedPercent: 12.5, ResetsAt: "2026-11-16T09:00:00Z"},
			{ID: "seven_day_fable", WindowMinutes: 10080, Scope: "Fable", UsedPercent: 0, ResetsAt: "2026-11-16T09:00:00Z"},
		},
		ExtraUsage: &control.HarnessUsageExtra{Enabled: false},
	}, view)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "must-not-return")

	args, err := os.ReadFile(r.command + ".args")
	require.NoError(t, err)
	assert.Equal(t, strings.Join(claudeUsageArgs, " ")+"\n", string(args))
	cwd, err := os.ReadFile(r.command + ".cwd")
	require.NoError(t, err)
	workDir, err := filepath.EvalSymlinks(r.workDir)
	require.NoError(t, err)
	assert.Equal(t, workDir, strings.TrimSpace(string(cwd)))
	info, err := os.Stat(r.workDir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	// stdin closes after the read and the CLI is allowed to exit on its own.
	require.Eventually(t, func() bool { _, err := os.Stat(r.command + ".exited"); return err == nil },
		2*time.Second, 10*time.Millisecond)
}

func TestClaudeUsageWindowValidation(t *testing.T) {
	scoped := []string{
		`{"display_name":"bad‮name","utilization":1}`,
		`{"display_name":"!!!","utilization":1}`,
		`"not-an-object"`,
		`{"display_name":"Opus 5.5 (Preview)","utilization":2,"resets_at":"2026-11-16T09:00:00.5-08:00"}`,
	}
	view := claudeUsageView(json.RawMessage(`{"subscription_type":"pro\u0007","rate_limits_available":true,"rate_limits":{` +
		`"five_hour":{"utilization":140,"resets_at":"not-a-time"},"seven_day":{"utilization":-1},` +
		`"seven_day_opus":{"utilization":"12"},"seven_day_sonnet":{"utilization":null},` +
		`"model_scoped":[` + strings.Join(scoped, ",") + `],"extra_usage":{"is_enabled":"yes"}}}`))
	assert.Equal(t, control.HarnessUsageView{State: control.HarnessUsageAvailable, Windows: []control.HarnessUsageWindow{
		{ID: "five_hour", WindowMinutes: 300, UsedPercent: 100},
		{ID: "seven_day_opus_5_5_preview", WindowMinutes: 10080, Scope: "Opus 5.5 (Preview)", UsedPercent: 2, ResetsAt: "2026-11-16T17:00:00Z"},
	}}, view)

	var many []string
	for i := range 20 {
		many = append(many, fmt.Sprintf(`{"display_name":"Model %d","utilization":%d}`, i, i))
	}
	view = claudeUsageView(json.RawMessage(`{"rate_limits_available":true,"rate_limits":{"model_scoped":[` + strings.Join(many, ",") + `]}}`))
	assert.Len(t, view.Windows, usageMaxWindows)
	for _, window := range view.Windows {
		assert.Regexp(t, `^[a-z0-9_]{1,64}$`, window.ID)
	}
}

func TestClaudeUsageUnsupportedWhenPlanLimitsDoNotApply(t *testing.T) {
	for _, body := range []string{
		`{"subscription_type":null,"rate_limits_available":false,"rate_limits":null}`,
		`{"subscription_type":"max","rate_limits_available":true,"rate_limits":null}`,
	} {
		view := claudeUsageView(json.RawMessage(body))
		assert.Equal(t, control.HarnessUsageUnsupported, view.State)
		assert.Empty(t, view.Windows)
		assert.Empty(t, view.ErrorCode)
	}
}

func TestClaudeUsageClassifiesNativeFailures(t *testing.T) {
	initOK := `IFS= read -r init
printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"pax-usage-init","response":{}}}'
IFS= read -r usage
`
	for name, tc := range map[string]struct{ script, code string }{
		"older CLI":    {initOK + `printf '%s\n' '{"type":"control_response","response":{"subtype":"error","request_id":"pax-usage-read","error":"Unsupported control request subtype: get_usage"}}'`, "cli_unsupported"},
		"native error": {initOK + `printf '%s\n' '{"type":"control_response","response":{"subtype":"error","request_id":"pax-usage-read","error":"must-not-return"}}'`, "read_failed"},
		"initialize error": {`IFS= read -r init
printf '%s\n' '{"type":"control_response","response":{"subtype":"error","request_id":"pax-usage-init","error":"must-not-return"}}'`, "read_failed"},
		"malformed payload": {initOK + `printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"pax-usage-read","response":"must-not-return"}}'`, "read_failed"},
		"exits early":       {`echo must-not-return; exit 1`, "read_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			view, err := usageReader(t, tc.script, nil).Usage(t.Context(), claudeQuery)
			require.NoError(t, err)
			assert.Equal(t, control.HarnessUsageUnavailable, view.State)
			assert.Equal(t, tc.code, view.ErrorCode)
			assert.NotEmpty(t, view.ObservedAt)
			raw, err := json.Marshal(view)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "must-not-return")
		})
	}
}

func codexUsageScript(account, rateLimits string) string {
	return `[ "$1" = app-server ] || exit 2
pwd -P > "$0.cwd"
IFS= read -r line
printf '%s\n' '{"id":1,"result":{}}'
IFS= read -r line
IFS= read -r line
case "$line" in *'"method":"account/read"'*'"refreshToken":false'*) ;; *) exit 3 ;; esac
printf '%s\n' '` + account + `'
IFS= read -r line
case "$line" in *'"method":"account/rateLimits/read"'*) ;; *) exit 4 ;; esac
printf '%s\n' '{"method":"account/rateLimits/updated","params":{}}'
printf '%s\n' '` + rateLimits + `'
cat >/dev/null
`
}

const codexChatGPTAccount = `{"id":2,"result":{"account":{"type":"chatgpt","email":"must-not-return@example.test","planType":"plus"}}}`

func TestCodexUsageReadsChatGPTRateLimits(t *testing.T) {
	r := usageReader(t, codexUsageScript(codexChatGPTAccount, `{"id":3,"result":{"rateLimits":{"limitId":"codex",`+
		`"primary":{"usedPercent":31,"windowDurationMins":10080,"resetsAt":1794992298},"secondary":null,`+
		`"credits":{"hasCredits":true,"balance":"must-not-return"},"planType":"pro"}}}`), nil)
	view, err := r.Usage(t.Context(), control.HarnessUsageStatusQuery{Harness: "codex"})
	require.NoError(t, err)
	assert.Equal(t, control.HarnessUsageAvailable, view.State)
	assert.Equal(t, "pro", view.Plan)
	assert.Equal(t, []control.HarnessUsageWindow{
		{ID: "primary", WindowMinutes: 10080, UsedPercent: 31, ResetsAt: "2026-11-18T08:58:18Z"},
	}, view.Windows)
	assert.Nil(t, view.ExtraUsage)
	raw, err := json.Marshal(view)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "must-not-return")
	cwd, err := os.ReadFile(r.command + ".cwd")
	require.NoError(t, err)
	workDir, err := filepath.EvalSymlinks(r.workDir)
	require.NoError(t, err)
	assert.Equal(t, workDir, strings.TrimSpace(string(cwd)))
}

func TestCodexUsageFallsBackToAccountPlan(t *testing.T) {
	r := usageReader(t, codexUsageScript(codexChatGPTAccount, `{"id":3,"result":{"rateLimits":{`+
		`"primary":{"usedPercent":5,"windowDurationMins":300},"secondary":{"usedPercent":1,"windowDurationMins":null}}}}`), nil)
	view, err := r.Usage(t.Context(), control.HarnessUsageStatusQuery{Harness: "codex"})
	require.NoError(t, err)
	assert.Equal(t, "plus", view.Plan)
	assert.Equal(t, []control.HarnessUsageWindow{{ID: "primary", WindowMinutes: 300, UsedPercent: 5}}, view.Windows)
}

func TestCodexUsageStates(t *testing.T) {
	for name, tc := range map[string]struct{ account, rateLimits, state, code string }{
		"api key":        {`{"id":2,"result":{"account":{"type":"apiKey"}}}`, `{}`, control.HarnessUsageUnsupported, ""},
		"signed out":     {`{"id":2,"result":{"account":null}}`, `{}`, control.HarnessUsageUnsupported, ""},
		"older server":   {codexChatGPTAccount, `{"id":3,"error":{"code":-32600,"message":"unknown variant"}}`, control.HarnessUsageUnavailable, "cli_unsupported"},
		"provider error": {codexChatGPTAccount, `{"id":3,"error":{"code":-32000,"message":"must-not-return"}}`, control.HarnessUsageUnavailable, "read_failed"},
		"no limits":      {codexChatGPTAccount, `{"id":3,"result":{"rateLimits":null}}`, control.HarnessUsageUnavailable, "read_failed"},
		"account error":  {`{"id":2,"error":{"code":-32000}}`, `{}`, control.HarnessUsageUnavailable, "read_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			view, err := usageReader(t, codexUsageScript(tc.account, tc.rateLimits), nil).
				Usage(t.Context(), control.HarnessUsageStatusQuery{Harness: "codex"})
			require.NoError(t, err)
			assert.Equal(t, tc.state, view.State)
			assert.Equal(t, tc.code, view.ErrorCode)
			assert.Empty(t, view.Windows)
		})
	}
}

func TestUsageSharesOneReadAndHonorsCacheTTL(t *testing.T) {
	clock := &usageClock{now: time.Unix(1_800_000_000, 0)}
	r := usageReader(t, claudeUsageScript(claudeUsageBody), clock.Now)
	require.NoError(t, os.WriteFile(r.command+".hold", nil, 0o600))
	var wg sync.WaitGroup
	views := make([]control.HarnessUsageView, 5)
	for i := range views {
		wg.Add(1)
		go func() {
			defer wg.Done()
			view, err := r.Usage(t.Context(), claudeQuery)
			assert.NoError(t, err)
			views[i] = view
		}()
	}
	require.Eventually(t, func() bool { return usageCalls(t, r) == 1 }, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, os.WriteFile(r.command+".release", nil, 0o600))
	wg.Wait()
	for _, view := range views {
		assert.Equal(t, control.HarnessUsageAvailable, view.State)
	}
	require.NoError(t, os.Remove(r.command+".hold"))

	_, err := r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)
	clock.Advance(59 * time.Second)
	_, err = r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)
	assert.Equal(t, 1, usageCalls(t, r))
	clock.Advance(2 * time.Second)
	_, err = r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)
	assert.Equal(t, 2, usageCalls(t, r))
}

func TestUsageRetriesFailuresSooner(t *testing.T) {
	clock := &usageClock{now: time.Unix(1_800_000_000, 0)}
	r := usageReader(t, "exit 1\n", clock.Now)
	for range 2 {
		view, err := r.Usage(t.Context(), claudeQuery)
		require.NoError(t, err)
		assert.Equal(t, "read_failed", view.ErrorCode)
	}
	assert.Equal(t, 1, usageCalls(t, r))
	clock.Advance(16 * time.Second)
	_, err := r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)
	assert.Equal(t, 2, usageCalls(t, r))
}

func TestUsageCallerCancellationDoesNotAbortSharedRead(t *testing.T) {
	r := usageReader(t, claudeUsageScript(claudeUsageBody), nil)
	require.NoError(t, os.WriteFile(r.command+".hold", nil, 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	cancelled := make(chan error, 1)
	go func() {
		_, err := r.Usage(ctx, claudeQuery)
		cancelled <- err
	}()
	require.Eventually(t, func() bool { return usageCalls(t, r) == 1 }, 2*time.Second, 5*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-cancelled, context.Canceled)

	joined := make(chan control.HarnessUsageView, 1)
	go func() {
		view, err := r.Usage(t.Context(), claudeQuery)
		assert.NoError(t, err)
		joined <- view
	}()
	require.NoError(t, os.WriteFile(r.command+".release", nil, 0o600))
	select {
	case view := <-joined:
		assert.Equal(t, control.HarnessUsageAvailable, view.State)
	case <-time.After(3 * time.Second):
		t.Fatal("joined read did not finish")
	}
	assert.Equal(t, 1, usageCalls(t, r))
}

func TestUsageTimeoutStopsTheNativeProcessGroup(t *testing.T) {
	r := usageReader(t, `IFS= read -r init
printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"pax-usage-init","response":{}}}'
sleep 30 &
wait
`, nil)
	r.claudeTimeout = 300 * time.Millisecond
	started := time.Now()
	view, err := r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)
	assert.Equal(t, control.HarnessUsageUnavailable, view.State)
	assert.Equal(t, "timeout", view.ErrorCode)
	r.Close()
	assert.Less(t, time.Since(started), 2*time.Second)
}

func TestUsageStopsACLIThatIgnoresStdinCloseAfterTheGracePeriod(t *testing.T) {
	r := usageReader(t, "echo $$ > \"$0.pid\"\n"+claudeUsageScript(claudeUsageBody)+"sleep 30\n", nil)
	view, err := r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)
	assert.Equal(t, control.HarnessUsageAvailable, view.State)
	raw, err := os.ReadFile(r.command + ".pid")
	require.NoError(t, err)
	var pid int
	_, err = fmt.Sscan(string(raw), &pid)
	require.NoError(t, err)
	process, err := os.FindProcess(pid)
	require.NoError(t, err)
	// The CLI may exit on its own first; one that keeps running is stopped and
	// reaped once the grace period ends, without waiting for Close.
	require.NoError(t, process.Signal(syscall.Signal(0)))
	require.Eventually(t, func() bool { return process.Signal(syscall.Signal(0)) != nil },
		r.exitGrace+2*time.Second, 20*time.Millisecond)
}

func TestUsageRequiresAPrivateWorkDir(t *testing.T) {
	r := usageReader(t, claudeUsageScript(claudeUsageBody), nil)
	r.workDir = ""
	view, err := r.Usage(t.Context(), claudeQuery)
	require.NoError(t, err)
	assert.Equal(t, "read_failed", view.ErrorCode)
	assert.Equal(t, 0, usageCalls(t, r))
}

// TestRealNativeUsageRead is opt-in: it reads plan usage through the installed
// CLIs. No model request is made, but the CLIs contact their providers.
func TestRealNativeUsageRead(t *testing.T) {
	if os.Getenv("PAXD_TEST_REAL_USAGE") == "" {
		t.Skip("set PAXD_TEST_REAL_USAGE=1 to read plan usage through installed claude and codex CLIs")
	}
	r := NewUsageReader(t.Context(), UsageOptions{WorkDir: filepath.Join(t.TempDir(), "work")})
	t.Cleanup(r.Close)
	for _, harness := range []string{"claude", "codex"} {
		started := time.Now()
		view, err := r.Usage(t.Context(), control.HarnessUsageStatusQuery{Harness: harness})
		require.NoError(t, err)
		raw, err := json.Marshal(view)
		require.NoError(t, err)
		t.Logf("%s read in %s: %s", harness, time.Since(started).Round(time.Millisecond), raw)
		assert.NotEqual(t, control.HarnessUsageUnavailable, view.State)
	}
}

func TestUsageRejectsUnsupportedHarnessAndStoppedReader(t *testing.T) {
	r := usageReader(t, "exit 1\n", nil)
	_, err := r.Usage(t.Context(), control.HarnessUsageStatusQuery{Harness: "gemini"})
	require.Error(t, err)
	r.Close()
	_, err = r.Usage(t.Context(), claudeQuery)
	require.Error(t, err)
	assert.Equal(t, 0, usageCalls(t, r))
}
