package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
