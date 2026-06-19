package agentregistry

import (
	"os"
	"path/filepath"
	"strings"
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

func TestRegistryIncludesQwenCodeACP(t *testing.T) {
	agents, err := Default().Agents([]string{"qwen-code", "qwen_code"})
	if err != nil {
		t.Fatalf("Agents(qwen-code,qwen_code) error = %v", err)
	}
	if len(agents) != 2 || agents[0].Name != "qwen" || agents[1].Name != "qwen" {
		t.Fatalf("Agents(qwen aliases) = %+v, want qwen aliases to canonicalize", agents)
	}
	if got := agents[0].Command; len(got) != 1 || got[0] != "~/.qwen/projects" {
		t.Fatalf("qwen command = %#v, want ~/.qwen/projects", got)
	}
	if got := agents[0].InstallCommands; len(got) != 1 || strings.Join(got[0], " ") != "npm install -g @qwen-code/qwen-code" {
		t.Fatalf("qwen install commands = %#v", got)
	}
}

func TestListQwenLocalSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QWEN_HOME", dir)
	chats := filepath.Join(dir, "projects", "-tmp-project", "chats")
	if err := os.MkdirAll(chats, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	chat := filepath.Join(chats, "qwen-session-1.jsonl")
	if err := os.WriteFile(chat, []byte(
		`{"uuid":"u1","sessionId":"qwen-session-1","timestamp":"2026-06-19T16:38:45.242Z","type":"user","cwd":"/tmp/project","message":{"role":"user","parts":[{"text":"hello qwen"}]}}`+"\n"+
			`{"uuid":"u2","sessionId":"qwen-session-1","timestamp":"2026-06-19T16:39:30.021Z","type":"system","subtype":"ui_telemetry","systemPayload":{"uiEvent":{"model":"z-ai/glm-5.2","duration_ms":44729,"input_token_count":23147,"output_token_count":17,"cached_content_token_count":0,"thoughts_token_count":0,"total_token_count":23164,"response_text":"Hello!"}}}`+"\n"+
			`{"uuid":"u3","sessionId":"qwen-session-1","timestamp":"2026-06-19T16:39:30.022Z","type":"assistant","cwd":"/tmp/project","model":"z-ai/glm-5.2","message":{"role":"model","parts":[{"text":"Hello!"}]},"usageMetadata":{"promptTokenCount":23147,"candidatesTokenCount":17,"thoughtsTokenCount":0,"totalTokenCount":23164,"cachedContentTokenCount":0}}`+"\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	status := DetectWithProbe(Agent{Name: "qwen", Kind: "local", Command: []string{"~/.qwen/projects"}}, false)
	if !status.Available || status.Capability != "local-log" {
		t.Fatalf("qwen status = %+v, want local-log", status)
	}
	sessions, err := listQwenLocalSessions()
	if err != nil {
		t.Fatalf("listQwenLocalSessions() error = %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("len(sessions) = %d, want 1", len(sessions))
	}
	if sessions[0].SessionID != "qwen:qwen-session-1" || sessions[0].Name != "hello qwen" || sessions[0].ProjectID != "/tmp/project" {
		t.Fatalf("session = %+v, want qwen metadata", sessions[0])
	}
	elements, err := QwenLocalElements("qwen-session-1")
	if err != nil {
		t.Fatalf("QwenLocalElements() error = %v", err)
	}
	if len(elements) != 3 {
		t.Fatalf("len(elements) = %d, want 3", len(elements))
	}
	if elements[0].Type != "message" || elements[0].Role != "user" || elements[0].ContentText != "hello qwen" {
		t.Fatalf("first element = %+v, want user message", elements[0])
	}
	if elements[1].Type != "usage" || elements[1].DurationMS != 44729 || elements[1].UsageJSON == "" {
		t.Fatalf("usage element = %+v, want telemetry usage", elements[1])
	}
	if elements[2].Type != "message" || elements[2].Role != "assistant" || elements[2].Model != "z-ai/glm-5.2" {
		t.Fatalf("assistant element = %+v", elements[2])
	}
}

func TestDetectZCodeAppOnlySource(t *testing.T) {
	dir := t.TempDir()
	appRoot := filepath.Join(dir, "ZCode.app")
	if err := os.Mkdir(appRoot, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	status := detectApp(Agent{
		Name:     "zcode",
		Kind:     "app",
		AppNames: []string{appRoot},
	})
	if !status.Available || status.State != "installed" || status.Capability != "app" {
		t.Fatalf("zcode status = %+v, want installed app-only", status)
	}
	if len(status.Command) != 1 || status.Command[0] != appRoot {
		t.Fatalf("zcode command = %#v, want app path", status.Command)
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
