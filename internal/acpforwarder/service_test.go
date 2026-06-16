package acpforwarder

import (
	"testing"
	"time"
)

func TestTunnelURLFromHTTP(t *testing.T) {
	tests := []struct {
		name       string
		base       string
		tunnelPath string
		want       string
	}{
		{
			name:       "https",
			base:       "https://fleet.example.com",
			tunnelPath: "/api/v1/agent/tunnel",
			want:       "wss://fleet.example.com/api/v1/agent/tunnel",
		},
		{
			name:       "http with path",
			base:       "http://localhost:8080/base/",
			tunnelPath: "tunnel",
			want:       "ws://localhost:8080/base/tunnel",
		},
		{
			name:       "already websocket",
			base:       "wss://fleet.example.com",
			tunnelPath: "",
			want:       "wss://fleet.example.com/api/v1/agent/tunnel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tunnelURLFromHTTP(tt.base, tt.tunnelPath)
			if err != nil {
				t.Fatalf("tunnelURLFromHTTP() error = %v", err)
			}
			if got.String() != tt.want {
				t.Fatalf("tunnelURLFromHTTP() = %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestTunnelURLFromHTTPRejectsUnsupportedScheme(t *testing.T) {
	if _, err := tunnelURLFromHTTP("ftp://fleet.example.com", "/tunnel"); err == nil {
		t.Fatal("expected unsupported scheme error")
	}
}

func TestNextReconnectBackoffDoublesFailedConnections(t *testing.T) {
	initial := 2 * time.Second
	got := nextReconnectBackoff(initial, initial, false)
	if got != 4*time.Second {
		t.Fatalf("next backoff = %s, want 4s", got)
	}

	got = nextReconnectBackoff(20*time.Second, initial, false)
	if got != maxReconnectBackoff {
		t.Fatalf("capped backoff = %s, want %s", got, maxReconnectBackoff)
	}
}

func TestNextReconnectBackoffResetsAfterConnectedTunnel(t *testing.T) {
	initial := 2 * time.Second
	got := nextReconnectBackoff(maxReconnectBackoff, initial, true)
	if got != initial {
		t.Fatalf("reset backoff = %s, want %s", got, initial)
	}
}
