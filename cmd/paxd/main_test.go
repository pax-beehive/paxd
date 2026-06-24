package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/store"
)

func TestPostmanTunnelURL(t *testing.T) {
	got, err := postmanTunnelURL(
		"https://app.example.com",
		"/api/v1/user/self/agents/{agent_id}/tunnel",
		"agent_123",
	)
	if err != nil {
		t.Fatalf("postmanTunnelURL() error = %v", err)
	}
	want := "wss://app.example.com/api/v1/user/self/agents/agent_123/tunnel"
	if got != want {
		t.Fatalf("postmanTunnelURL() = %q, want %q", got, want)
	}
}

func TestPostmanTunnelURLPreservesBasePath(t *testing.T) {
	got, err := postmanTunnelURL("http://localhost:9879/base/", "tunnel/{agent_id}", "agent/123")
	if err != nil {
		t.Fatalf("postmanTunnelURL() error = %v", err)
	}
	want := "ws://localhost:9879/base/tunnel/agent%2F123"
	if got != want {
		t.Fatalf("postmanTunnelURL() = %q, want %q", got, want)
	}
}

func TestACPCommandForHarness(t *testing.T) {
	tests := []struct {
		harness string
		want    string
	}{
		{harness: "hermes", want: "hermes acp"},
		{harness: "codex", want: "@zed-industries/codex-acp"},
		{harness: "claude", want: "@agentclientprotocol/claude-agent-acp"},
		{harness: "claude_code", want: "@agentclientprotocol/claude-agent-acp"},
		{harness: "gemini", want: "gemini --acp"},
		{harness: "custom", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.harness, func(t *testing.T) {
			got := acpCommandForHarness(tt.harness)
			gotText := strings.Join(got, " ")
			if tt.want == "" {
				if gotText != "" {
					t.Fatalf("acpCommandForHarness(%q) = %#v, want empty", tt.harness, got)
				}
				return
			}
			if !strings.Contains(gotText, tt.want) {
				t.Fatalf("acpCommandForHarness(%q) = %#v, want to contain %q", tt.harness, got, tt.want)
			}
		})
	}
}

func TestParseConfigureAgentSpecsDefault(t *testing.T) {
	got, err := parseConfigureAgentSpecs(nil, "claude")
	if err != nil {
		t.Fatalf("parseConfigureAgentSpecs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Name != "claude-code" || got[0].Harness != "claude-code" || got[0].InstanceID != "default" {
		t.Fatalf("parseConfigureAgentSpecs() = %#v", got[0])
	}
}

func TestParseConfigureAgentSpecsMultiple(t *testing.T) {
	got, err := parseConfigureAgentSpecs(
		[]string{"work:codex:primary", "review:claude_code"},
		"gemini",
	)
	if err != nil {
		t.Fatalf("parseConfigureAgentSpecs() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].Name != "work" || got[0].Harness != "codex" || got[0].InstanceID != "primary" {
		t.Fatalf("got[0] = %#v", got[0])
	}
	if got[1].Name != "review" || got[1].Harness != "claude-code" || got[1].InstanceID != "review" {
		t.Fatalf("got[1] = %#v", got[1])
	}
}

func TestAgentACPForwarderConfigUsesPerAgentHarness(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		Cloud: config.CloudConfig{
			APIURL:         "https://app.example.com",
			APIKey:         "node_key",
			CFClientID:     "cf_id",
			CFClientSecret: "cf_secret",
		},
		ACPForwarder: config.ACPForwarderConfig{
			Enabled:           true,
			TunnelPath:        "/api/v1/agent/tunnel",
			ReconnectInterval: 2 * time.Second,
		},
	}
	node := &store.NodeState{
		CloudAPIURL: "https://node.example.com",
		CloudAPIKey: "stored_node_key",
	}
	agent := config.RuntimeAgentConfig{
		AgentID:    "agent_review",
		InstanceID: "review",
		AgentType:  "claude-code",
		Enabled:    &enabled,
		ACPForwarder: config.AgentACPForwarderConfig{
			Harness:           "claude-code",
			ReconnectInterval: 7 * time.Second,
		},
	}

	got, ok := agentACPForwarderConfig(cfg, node, agent)
	if !ok {
		t.Fatal("agentACPForwarderConfig() disabled, want enabled")
	}
	if got.AgentID != "agent_review" || got.InstanceID != "review" {
		t.Fatalf("agent identity = %s/%s", got.AgentID, got.InstanceID)
	}
	if got.ConnectionID != "review" {
		t.Fatalf("connection id = %q, want review", got.ConnectionID)
	}
	if got.CloudURL != "https://node.example.com" || got.APIKey != "stored_node_key" {
		t.Fatalf("cloud auth = %s/%s", got.CloudURL, got.APIKey)
	}
	if got.ReconnectInterval != 7*time.Second {
		t.Fatalf("ReconnectInterval = %s", got.ReconnectInterval)
	}
	if command := strings.Join(got.Command, " "); !strings.Contains(command, "@agentclientprotocol/claude-agent-acp") {
		t.Fatalf("command = %#v", got.Command)
	}
}

func TestAgentACPForwarderConfigCanDisableOneAgent(t *testing.T) {
	disabled := false
	cfg := &config.Config{ACPForwarder: config.ACPForwarderConfig{Enabled: true}}
	agent := config.RuntimeAgentConfig{
		AgentID:   "agent_disabled",
		AgentType: "codex",
		ACPForwarder: config.AgentACPForwarderConfig{
			Enabled: &disabled,
		},
	}

	if _, ok := agentACPForwarderConfig(cfg, &store.NodeState{}, agent); ok {
		t.Fatal("agentACPForwarderConfig() enabled disabled agent")
	}
}

func TestACPSessionListerIncludesHermesAgents(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		Cloud: config.CloudConfig{
			APIURL: "https://app.example.com",
			APIKey: "node_key",
		},
		ACPForwarder: config.ACPForwarderConfig{Enabled: true},
		Agents: []config.RuntimeAgentConfig{{
			AgentID:   "agent_hermes",
			AgentType: "hermes",
			Enabled:   &enabled,
		}},
	}
	agent := store.CloudAgent{
		AgentID:   "agent_hermes",
		AgentType: "hermes",
	}

	got := acpSessionListerForAgent(cfg, agent)
	if got == nil {
		t.Fatal("acpSessionListerForAgent() = nil, want Hermes ACP lister")
	}
	if command := strings.Join(got.Command, " "); command != "hermes acp" {
		t.Fatalf("command = %q, want hermes acp", command)
	}
}

func TestSyncConfiguredAgentsDisablesStaleCloudAgents(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "paxd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.SaveCloudAgent(&store.CloudAgent{
		AgentID:    "agent_stale",
		InstanceID: "stale",
		Name:       "stale",
		AgentType:  "codex",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("save stale agent: %v", err)
	}

	enabled := true
	cfg := &config.Config{
		Agents: []config.RuntimeAgentConfig{{
			AgentID:    "agent_current",
			InstanceID: "current",
			Name:       "current",
			AgentType:  "codex",
			Enabled:    &enabled,
		}},
	}

	if err := syncConfiguredAgents(cfg, db); err != nil {
		t.Fatalf("syncConfiguredAgents() error = %v", err)
	}
	agents, err := db.ListCloudAgents()
	if err != nil {
		t.Fatalf("ListCloudAgents() error = %v", err)
	}
	if len(agents) != 1 || agents[0].AgentID != "agent_current" {
		t.Fatalf("enabled agents = %#v, want only agent_current", agents)
	}
}

func TestSyncConfiguredAgentsDisablesDBAgentsWhenConfigHasNoAgentIDs(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "paxd.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	if err := db.SaveCloudAgent(&store.CloudAgent{
		AgentID:    "agent_saved",
		InstanceID: "saved",
		Name:       "saved",
		AgentType:  "codex",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("save cloud agent: %v", err)
	}

	cfg := &config.Config{}
	if err := syncConfiguredAgents(cfg, db); err != nil {
		t.Fatalf("syncConfiguredAgents() error = %v", err)
	}
	agents, err := db.ListCloudAgents()
	if err != nil {
		t.Fatalf("ListCloudAgents() error = %v", err)
	}
	if len(agents) != 0 {
		t.Fatalf("enabled agents = %#v, want none because config has no agent IDs", agents)
	}
}

func TestYAMLMarshalOmitsLegacyAgentShorthandForMultiAgentConfig(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		AgentID:    "legacy_agent",
		InstanceID: "legacy_instance",
		Agent: config.AgentConfig{
			AgentID:     "legacy_agent",
			Name:        "node",
			MachineType: "unknown",
			Hostname:    "host",
		},
		Cloud: config.CloudConfig{
			APIURL: "https://app.example.com",
			NodeID: "node_123",
			APIKey: "pax_key",
		},
		Agents: []config.RuntimeAgentConfig{{
			AgentID:    "agent_codex",
			InstanceID: "codex-main",
			Name:       "codex-main",
			AgentType:  "codex",
			Enabled:    &enabled,
		}},
	}

	data, err := yamlMarshal(cfg)
	if err != nil {
		t.Fatalf("yamlMarshal() error = %v", err)
	}
	text := string(data)
	for _, legacy := range []string{
		"\nagent_id: \"legacy_agent\"",
		"instance_id: \"legacy_instance\"",
		"agent:\n  agent_id:",
	} {
		if strings.Contains(text, legacy) {
			t.Fatalf("yaml contains legacy shorthand %q:\n%s", legacy, text)
		}
	}
	if !strings.Contains(text, "agents:\n  - agent_id: \"agent_codex\"") {
		t.Fatalf("yaml missing agents list:\n%s", text)
	}
}

func TestYAMLMarshalOmitsHermesEndpointForACPOnlyAgents(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		Agent: config.AgentConfig{Name: "node"},
		Cloud: config.CloudConfig{
			APIURL: "https://app.example.com",
			NodeID: "node_123",
			APIKey: "pax_key",
		},
		Hermes: config.HermesConfig{
			APIEndpoint: "http://localhost:8642",
			APIKeyEnv:   "~/.hermes/.env",
			Profile:     "work",
		},
		Agents: []config.RuntimeAgentConfig{{
			AgentID:    "agent_codex",
			InstanceID: "codex-main",
			Name:       "codex-main",
			AgentType:  "codex",
			Enabled:    &enabled,
		}},
	}

	data, err := yamlMarshal(cfg)
	if err != nil {
		t.Fatalf("yamlMarshal() error = %v", err)
	}
	text := string(data)
	for _, unexpected := range []string{
		"hermes:",
		"api_endpoint:",
		"api_key_from_env:",
		"profile:",
		"http://localhost:8642",
	} {
		if strings.Contains(text, unexpected) {
			t.Fatalf("yaml contains %q:\n%s", unexpected, text)
		}
	}
}

func TestYAMLMarshalKeepsHermesEndpointForHermesAgent(t *testing.T) {
	enabled := true
	cfg := &config.Config{
		Agent: config.AgentConfig{Name: "node"},
		Cloud: config.CloudConfig{
			APIURL: "https://app.example.com",
			NodeID: "node_123",
			APIKey: "pax_key",
		},
		Hermes: config.HermesConfig{
			APIEndpoint: "http://localhost:8642",
			APIKeyEnv:   "~/.hermes/.env",
		},
		Agents: []config.RuntimeAgentConfig{{
			AgentID:     "agent_hermes",
			InstanceID:  "hermes-main",
			Name:        "hermes-main",
			AgentType:   "hermes",
			APIEndpoint: "http://localhost:8642",
			APIKeyEnv:   "~/.hermes/.env",
			Enabled:     &enabled,
		}},
	}

	data, err := yamlMarshal(cfg)
	if err != nil {
		t.Fatalf("yamlMarshal() error = %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "hermes:") || !strings.Contains(text, "api_endpoint: \"http://localhost:8642\"") {
		t.Fatalf("yaml missing hermes endpoint:\n%s", text)
	}
}

func TestConfigureHermesAPIEndpointOnlyForHermesSpecs(t *testing.T) {
	cfg := &config.Config{Hermes: config.HermesConfig{APIEndpoint: "http://localhost:8642"}}
	if got := configureHermesAPIEndpoint(cfg, []configureAgentSpec{{Harness: "codex"}}); got != "" {
		t.Fatalf("configureHermesAPIEndpoint(codex) = %q", got)
	}
	if got := configureHermesAPIEndpoint(cfg, []configureAgentSpec{{Harness: "hermes"}}); got != "http://localhost:8642" {
		t.Fatalf("configureHermesAPIEndpoint(hermes) = %q", got)
	}
}

func TestConfigureWouldReplaceExistingAgents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "paxd.yaml")
	cfg := &config.Config{
		Agents: []config.RuntimeAgentConfig{{AgentID: "agent_existing"}},
	}

	if configureWouldReplaceExistingAgents(path, cfg) {
		t.Fatal("configureWouldReplaceExistingAgents() = true for missing file")
	}
	if err := os.WriteFile(path, []byte("agents: []\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if !configureWouldReplaceExistingAgents(path, cfg) {
		t.Fatal("configureWouldReplaceExistingAgents() = false for existing agents")
	}
	if configureWouldReplaceExistingAgents(path, &config.Config{}) {
		t.Fatal("configureWouldReplaceExistingAgents() = true for no configured agents")
	}
}

func TestExplicitAgentsForAppendPreservesExistingAgents(t *testing.T) {
	cfg := &config.Config{
		Agents: []config.RuntimeAgentConfig{{
			AgentID:    "agent_existing",
			InstanceID: "existing",
			AgentType:  "codex",
		}},
	}

	got := explicitAgentsForAppend(cfg)
	if len(got) != 1 || got[0].AgentID != "agent_existing" {
		t.Fatalf("explicitAgentsForAppend() = %#v", got)
	}
	got[0].AgentID = "mutated"
	if cfg.Agents[0].AgentID != "agent_existing" {
		t.Fatalf("explicitAgentsForAppend() returned backing slice")
	}
}

func TestExplicitAgentsForAppendConvertsLegacyAgent(t *testing.T) {
	cfg := &config.Config{
		AgentID:    "agent_legacy",
		InstanceID: "legacy",
		Agent: config.AgentConfig{
			AgentID: "agent_legacy",
			Name:    "legacy-agent",
		},
		Hermes: config.HermesConfig{
			APIEndpoint: "http://localhost:8642",
		},
	}

	got := explicitAgentsForAppend(cfg)
	if len(got) != 1 {
		t.Fatalf("len(explicitAgentsForAppend()) = %d", len(got))
	}
	if got[0].AgentID != "agent_legacy" || got[0].InstanceID != "legacy" || got[0].AgentType != "hermes" {
		t.Fatalf("explicitAgentsForAppend() = %#v", got[0])
	}
}
