package agentregistry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalSessionID(t *testing.T) {
	if got := CanonicalSessionID("codex", "abc"); got != "codex:abc" {
		t.Fatalf("CanonicalSessionID() = %q, want codex:abc", got)
	}
	if got := CanonicalSessionID("codex", "claude:abc"); got != "claude:abc" {
		t.Fatalf("CanonicalSessionID() = %q, want existing composite id", got)
	}
}

func TestRegistryAgentsRejectsUnknownAgent(t *testing.T) {
	_, err := Default().Agents([]string{"unknown"})
	if err == nil {
		t.Fatal("Agents() error = nil, want unknown agent error")
	}
}

func TestRegistryIncludesPiACP(t *testing.T) {
	agents, err := Default().Agents([]string{"pi"})
	if err != nil {
		t.Fatalf("Agents(pi) error = %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "pi" {
		t.Fatalf("Agents(pi) = %+v, want pi", agents)
	}
	if got := agents[0].Command; len(got) != 1 || got[0] != "pi-acp" {
		t.Fatalf("pi command = %#v, want pi-acp", got)
	}
	if got := agents[0].FallbackCommand; len(got) != 3 || got[2] != "pi-acp" {
		t.Fatalf("pi fallback command = %#v, want npx -y pi-acp", got)
	}
}

func TestRegistryIncludesOpenClawGateway(t *testing.T) {
	agents, err := Default().Agents([]string{"openclaw"})
	if err != nil {
		t.Fatalf("Agents(openclaw) error = %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "openclaw" {
		t.Fatalf("Agents(openclaw) = %+v, want openclaw", agents)
	}
	if agents[0].Kind != "gateway" || agents[0].Source != "gateway" {
		t.Fatalf("openclaw kind/source = %s/%s, want gateway/gateway", agents[0].Kind, agents[0].Source)
	}
}

func TestDetectOpenClawWithoutProbeIsFastInventory(t *testing.T) {
	status := DetectWithProbe(Agent{
		Name:        "openclaw",
		Kind:        "gateway",
		Command:     []string{"definitely-missing-openclaw-test-binary"},
		Source:      "gateway",
		InstallHint: "install openclaw",
	}, false)
	if status.State != "missing" {
		t.Fatalf("status.State = %q, want missing", status.State)
	}
}

func TestDetectDoesNotAutoRunFallbackCommand(t *testing.T) {
	status := DetectWithProbe(Agent{
		Name:            "codex",
		Kind:            "acp",
		Command:         []string{"definitely-missing-codex-acp-test-binary"},
		FallbackCommand: []string{"sh"},
		InstallHint:     "install codex-acp",
	}, false)
	if status.Available {
		t.Fatalf("status.Available = true, want false")
	}
	if status.State != "installable" {
		t.Fatalf("status.State = %q, want installable", status.State)
	}
	if len(status.Command) != 1 || status.Command[0] != "sh" {
		t.Fatalf("status.Command = %#v, want fallback command for display", status.Command)
	}
}

func TestListCodexLocalSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "sessions", "2026", "06", "19"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session_index.jsonl"), []byte(
		`{"id":"sess-1","thread_name":"Local Codex","updated_at":"2026-06-19T01:02:03Z"}`+"\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile(index) error = %v", err)
	}
	rollout := filepath.Join(dir, "sessions", "2026", "06", "19", "rollout-2026-06-19T01-02-03-sess-1.jsonl")
	if err := os.WriteFile(rollout, []byte(
		`{"type":"session_meta","payload":{"id":"sess-1","timestamp":"2026-06-19T01:02:03Z","cwd":"/tmp/project","source":"vscode"}}`+"\n"+
			`{"timestamp":"2026-06-19T01:02:04Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`+"\n"+
			`{"timestamp":"2026-06-19T01:02:05Z","type":"response_item","payload":{"type":"reasoning","encrypted_content":"abc"}}`+"\n"+
			`{"timestamp":"2026-06-19T01:02:06Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"pwd\"}","call_id":"call_1"}}`+"\n"+
			`{"timestamp":"2026-06-19T01:02:07Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"ok"}}`+"\n"+
			`{"timestamp":"2026-06-19T01:02:07Z","type":"event_msg","payload":{"type":"exec_command_end","call_id":"call_1","duration":{"secs":1,"nanos":250000000}}}`+"\n"+
			`{"timestamp":"2026-06-19T01:02:08Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":13},"last_token_usage":{"input_tokens":5,"cached_input_tokens":2,"output_tokens":3,"reasoning_output_tokens":1,"total_tokens":8},"model_context_window":100}}}`+"\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile(rollout) error = %v", err)
	}

	status := DetectWithProbe(Default().agents[0], false)
	if !status.Available || status.Capability != "local-log" {
		t.Fatalf("codex status = %+v, want available local-log", status)
	}
	sessions, err := listCodexLocalSessions()
	if err != nil {
		t.Fatalf("listCodexLocalSessions() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	if sessions[0].SessionID != "codex:sess-1" || sessions[0].Name != "Local Codex" || sessions[0].ProjectID != "/tmp/project" {
		t.Fatalf("session = %+v, want indexed rollout metadata", sessions[0])
	}
	elements, err := CodexLocalElements("sess-1")
	if err != nil {
		t.Fatalf("CodexLocalElements() error = %v", err)
	}
	if len(elements) != 5 {
		t.Fatalf("len(elements) = %d, want 5", len(elements))
	}
	if elements[0].Type != "message" || elements[0].Role != "user" || elements[0].ContentText != "hello" {
		t.Fatalf("message element = %+v", elements[0])
	}
	if elements[1].Type != "thinking" || elements[2].Type != "tool_call" || elements[3].Type != "tool_result" {
		t.Fatalf("element types = %s, %s, %s", elements[1].Type, elements[2].Type, elements[3].Type)
	}
	if elements[3].DurationMS != 1250 {
		t.Fatalf("tool result duration = %d, want 1250", elements[3].DurationMS)
	}
	if elements[2].NormalizedRaw["name"] != "exec_command" || elements[2].NormalizedRaw["callId"] != "call_1" {
		t.Fatalf("tool call normalized metadata = %+v", elements[2].NormalizedRaw)
	}
	args, ok := elements[2].NormalizedRaw["arguments"].(map[string]any)
	if !ok || args["cmd"] != "pwd" {
		t.Fatalf("tool call arguments = %#v, want decoded cmd", elements[2].NormalizedRaw["arguments"])
	}
	if elements[3].NormalizedRaw["callId"] != "call_1" || elements[3].NormalizedRaw["output"] != "ok" {
		t.Fatalf("tool result normalized metadata = %+v", elements[3].NormalizedRaw)
	}
	if elements[4].Type != "usage" || elements[4].UsageJSON == "" {
		t.Fatalf("usage element = %+v, want token usage", elements[4])
	}
}

func TestDecodeOpenClawSession(t *testing.T) {
	got := decodeOpenClawSession([]byte(`{
		"agentId":"work",
		"key":"agent:work:main",
		"label":"Work chat",
		"model":"gpt-5",
		"cwd":"/tmp/project",
		"updatedAt":"2026-06-18T12:00:00Z"
	}`))
	if got.SessionID != "openclaw:agent:work:main" {
		t.Fatalf("SessionID = %q, want openclaw:agent:work:main", got.SessionID)
	}
	if got.AgentType != "openclaw" || got.NativeID != "agent:work:main" {
		t.Fatalf("agent/native = %s/%s, want openclaw/agent:work:main", got.AgentType, got.NativeID)
	}
	if got.Name != "Work chat" || got.ProjectID != "/tmp/project" {
		t.Fatalf("name/project = %q/%q, want Work chat//tmp/project", got.Name, got.ProjectID)
	}
}
