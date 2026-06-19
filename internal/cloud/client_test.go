package cloud

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterNodeAgentDoesNotFollowRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/node/agents/register" {
			http.Redirect(w, r, "/cdn-cgi/access/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>login</html>"))
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "").RegisterNodeAgent(
		&RegisterNodeAgentRequest{Agent: RegisterNodeAgentPayload{Name: "codex", AgentType: "codex"}},
		"token",
	)
	if err == nil {
		t.Fatal("RegisterNodeAgent() error = nil, want redirect error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "status 302") {
		t.Fatalf("error %q does not include status 302", msg)
	}
	if !strings.Contains(msg, "/cdn-cgi/access/login") {
		t.Fatalf("error %q does not include redirect location", msg)
	}
}

func TestRegisterNodeAgentSendsCloudflareAccessHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("CF-Access-Client-Id"); got != "cf_id" {
			t.Errorf("CF-Access-Client-Id = %q", got)
		}
		if got := r.Header.Get("CF-Access-Client-Secret"); got != "cf_secret" {
			t.Errorf("CF-Access-Client-Secret = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"node_id":"node_1","api_key":"key_1","agent_id":"agent_1"}}`))
	}))
	defer server.Close()

	resp, err := NewClient(server.URL, "").
		WithCloudflareAccess("cf_id", "cf_secret").
		RegisterNodeAgent(
			&RegisterNodeAgentRequest{Agent: RegisterNodeAgentPayload{Name: "codex", AgentType: "codex"}},
			"token",
		)
	if err != nil {
		t.Fatalf("RegisterNodeAgent() error = %v", err)
	}
	if resp.AgentID != "agent_1" {
		t.Fatalf("AgentID = %q", resp.AgentID)
	}
}
