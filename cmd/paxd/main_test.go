package main

import (
	"strings"
	"testing"
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
