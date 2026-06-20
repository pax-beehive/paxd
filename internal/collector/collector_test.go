package collector

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/acpclient"
	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/hermes"
	"github.com/pax-beehive/paxd/internal/store"
)

func TestSplitReportsBatchesSessions(t *testing.T) {
	report := &cloud.NodeStatusReport{
		Hostname: "host",
		Agents: []cloud.AgentStatus{
			{AgentID: "agent-empty", Online: true},
			{AgentID: "agent-1", Online: true, Sessions: sessions("a", 3)},
			{AgentID: "agent-2", Online: true, Sessions: sessions("b", 2)},
		},
	}

	reports := splitReports(report, 2)
	if len(reports) != 3 {
		t.Fatalf("splitReports returned %d reports, want 3", len(reports))
	}

	total := 0
	for i, report := range reports {
		count := sessionCount(report)
		if count > 2 {
			t.Fatalf("batch %d session count = %d, want <= 2", i, count)
		}
		total += count
	}
	if total != 5 {
		t.Fatalf("total sessions = %d, want 5", total)
	}
	if reports[0].Agents[0].AgentID != "agent-empty" {
		t.Fatalf("empty-session agent was not preserved in first batch: %+v", reports[0].Agents)
	}
}

func TestCollectSessionsPrefersHermesHTTPForHermesAgents(t *testing.T) {
	var gotReport cloud.NodeStatusReport
	cloudServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/node/status" {
			t.Fatalf("unexpected cloud path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReport); err != nil {
			t.Fatalf("decode status report: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cloudServer.Close)

	hermesServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/sessions" {
			t.Fatalf("unexpected Hermes path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[
			{"sessionId":"hermes-session-1","name":"Hermes Session","status":"running","tokenUsage":42}
		]`))
	}))
	t.Cleanup(hermesServer.Close)

	acpCommand := writeEmptyACPSessionLister(t)
	runtime := AgentRuntime{
		Agent: store.CloudAgent{
			AgentID:   "agent-1",
			Name:      "Hermes",
			AgentType: "hermes",
		},
		HermesClient:       hermes.NewClient(hermesServer.URL, ""),
		SupportsHermesHTTP: true,
		ACPSessionLister: &acpclient.SessionLister{
			Command: []string{acpCommand},
			Timeout: time.Second,
		},
	}

	collector := New([]AgentRuntime{runtime}, cloud.NewClient(cloudServer.URL, ""), nil, "host")
	if err := collector.CollectAndReport(t.Context()); err != nil {
		t.Fatalf("CollectAndReport: %v", err)
	}

	if len(gotReport.Agents) != 1 {
		t.Fatalf("reported agents = %d, want 1", len(gotReport.Agents))
	}
	reportedSessions := gotReport.Agents[0].Sessions
	if len(reportedSessions) != 1 {
		t.Fatalf("reported sessions = %d, want 1: %+v", len(reportedSessions), reportedSessions)
	}
	if reportedSessions[0].SessionID != "hermes-session-1" {
		t.Fatalf("session id = %q, want hermes-session-1", reportedSessions[0].SessionID)
	}
	if reportedSessions[0].TokenUsage != 42 {
		t.Fatalf("token usage = %d, want 42", reportedSessions[0].TokenUsage)
	}
}

func sessions(prefix string, count int) []cloud.SessionStatus {
	out := make([]cloud.SessionStatus, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, cloud.SessionStatus{SessionID: fmt.Sprintf("%s%d", prefix, i)})
	}
	return out
}

func sessionCount(report *cloud.NodeStatusReport) int {
	total := 0
	for _, agent := range report.Agents {
		total += len(agent.Sessions)
	}
	return total
}

func writeEmptyACPSessionLister(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "empty-acp")
	script := `#!/bin/sh
while IFS= read -r line; do
	case "$line" in
		*"\"method\":\"initialize\""*)
			printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"authMethods":[]}}'
			;;
		*"\"method\":\"session/list\""*)
			printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"sessions":[]}}'
			;;
	esac
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write ACP session lister: %v", err)
	}
	return path
}
