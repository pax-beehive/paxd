package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaultsCloudAPIURL(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Cloud.APIURL != DefaultCloudAPIURL {
		t.Fatalf("Cloud.APIURL = %q, want %q", cfg.Cloud.APIURL, DefaultCloudAPIURL)
	}
	if cfg.Cloud.URL != DefaultCloudAPIURL {
		t.Fatalf("Cloud.URL = %q, want %q", cfg.Cloud.URL, DefaultCloudAPIURL)
	}
}

func TestLoadDetectsMachineNameAndPreservesOverride(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Agent.MachineType == "" || cfg.Agent.MachineType == "unknown" {
			t.Fatalf("MachineType = %q, want detected name", cfg.Agent.MachineType)
		}
	})

	t.Run("environment override", func(t *testing.T) {
		t.Setenv("PAX_MACHINE_TYPE", "Build Server")
		cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.Agent.MachineType != "Build Server" {
			t.Fatalf("MachineType = %q", cfg.Agent.MachineType)
		}
	})
}

func TestLoadAcceptsDeploymentAliases(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "paxd.yaml")
	body := []byte(`
cloud:
  url: https://app.example.com
  api_key: pax_key
agent_id: agent_123
instance_id: default
acp_forwarder:
  command: ["hermes", "acp"]
`)
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Cloud.APIURL != "https://app.example.com" {
		t.Fatalf("Cloud.APIURL = %q", cfg.Cloud.APIURL)
	}
	if cfg.Agent.AgentID != "agent_123" {
		t.Fatalf("Agent.AgentID = %q", cfg.Agent.AgentID)
	}
	if got := cfg.RuntimeAgents()[0].InstanceID; got != "default" {
		t.Fatalf("RuntimeAgents()[0].InstanceID = %q", got)
	}
}

func TestLoadAcceptsSmokeTestEnvAliases(t *testing.T) {
	t.Setenv("PAX_CLOUD_URL", "https://app.example.com")
	t.Setenv("PAX_API_KEY", "pax_key")
	t.Setenv("PAX_AGENT_ID", "agent_123")
	t.Setenv("PAX_INSTANCE_ID", "workstation")
	t.Setenv("PAX_CLOUD_CF_CLIENT_ID", "cf_id")
	t.Setenv("PAX_CLOUD_CF_CLIENT_SECRET", "cf_secret")
	t.Setenv("PAX_ACP_HARNESS", "codex")
	t.Setenv("PAX_ACP_RECONNECT_INTERVAL", "5s")

	cfg, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Cloud.APIURL != "https://app.example.com" {
		t.Fatalf("Cloud.APIURL = %q", cfg.Cloud.APIURL)
	}
	if cfg.Cloud.APIKey != "pax_key" {
		t.Fatalf("Cloud.APIKey = %q", cfg.Cloud.APIKey)
	}
	if cfg.Agent.AgentID != "agent_123" {
		t.Fatalf("Agent.AgentID = %q", cfg.Agent.AgentID)
	}
	if cfg.Cloud.CFClientID != "cf_id" || cfg.Cloud.CFClientSecret != "cf_secret" {
		t.Fatalf("Cloudflare service token aliases were not applied")
	}
	if cfg.ACPForwarder.Harness != "codex" {
		t.Fatalf("ACPForwarder.Harness = %q", cfg.ACPForwarder.Harness)
	}
	if cfg.ACPForwarder.ReconnectInterval != 5*time.Second {
		t.Fatalf("ReconnectInterval = %s", cfg.ACPForwarder.ReconnectInterval)
	}
	if got := cfg.RuntimeAgents()[0].InstanceID; got != "workstation" {
		t.Fatalf("RuntimeAgents()[0].InstanceID = %q", got)
	}
}

func TestLoadAcceptsPerAgentACPForwarderConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "paxd.yaml")
	body := []byte(`
cloud:
  api_url: https://app.example.com
  api_key: pax_key
acp_forwarder:
  enabled: true
  tunnel_path: /api/v1/agent/tunnel
agents:
  - agent_id: agent_codex
    instance_id: codex-main
    name: codex-main
    agent_type: codex
    enabled: true
    acp_forwarder:
      harness: codex
      command: ["codex-acp"]
  - agent_id: agent_review
    instance_id: review
    name: reviewer
    agent_type: claude-code
    enabled: true
    acp_forwarder:
      harness: claude-code
      command: ["claude-agent-acp"]
      reconnect_interval: 7s
`)
	if err := os.WriteFile(configPath, body, 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	agents := cfg.RuntimeAgents()
	if len(agents) != 2 {
		t.Fatalf("len(RuntimeAgents()) = %d, want 2", len(agents))
	}
	if got := agents[0].ACPForwarder.Command; len(got) != 1 || got[0] != "codex-acp" {
		t.Fatalf("codex command = %#v", got)
	}
	if agents[1].ACPForwarder.Harness != "claude-code" {
		t.Fatalf("claude harness = %q", agents[1].ACPForwarder.Harness)
	}
	if agents[1].ACPForwarder.ReconnectInterval != 7*time.Second {
		t.Fatalf("claude reconnect = %s", agents[1].ACPForwarder.ReconnectInterval)
	}
}

func TestDefaultConfigLogSettings(t *testing.T) {
	cfg := DefaultConfig()
	home, _ := os.UserHomeDir()
	if got, want := cfg.Daemon.LogFile, filepath.Join(home, ".paxd", "logs", "paxd.log"); got != want {
		t.Fatalf("Daemon.LogFile = %q, want %q", got, want)
	}
	if cfg.Daemon.LogMaxSizeMB != 20 {
		t.Fatalf("Daemon.LogMaxSizeMB = %d, want 20", cfg.Daemon.LogMaxSizeMB)
	}
	if cfg.Daemon.LogMaxBackups != 3 {
		t.Fatalf("Daemon.LogMaxBackups = %d, want 3", cfg.Daemon.LogMaxBackups)
	}
}
