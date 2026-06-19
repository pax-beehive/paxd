package collector

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/store"
	"github.com/pax-beehive/paxd/pkg/model"
)

type fakeHermesSessions struct {
	sessions []model.SessionInfo
	err      error
}

func (f fakeHermesSessions) GetSessions() ([]model.SessionInfo, error) {
	return f.sessions, f.err
}

func (f fakeHermesSessions) Ping() error {
	return nil
}

type fakeACPSessions struct {
	sessions []model.SessionInfo
	err      error
}

func (f fakeACPSessions) List(context.Context) ([]model.SessionInfo, error) {
	return f.sessions, f.err
}

func TestCollectSessionsPrefersHermesHTTPForHermesAgents(t *testing.T) {
	col := &Collector{}
	runtime := AgentRuntime{
		Agent:              store.CloudAgent{AgentID: "agent-1", AgentType: "hermes"},
		SupportsHermesHTTP: true,
		HermesClient: fakeHermesSessions{sessions: []model.SessionInfo{{
			SessionID:  "sess-http",
			AgentType:  "hermes",
			TokenUsage: 42,
		}}},
		ACPSessionLister: fakeACPSessions{sessions: []model.SessionInfo{{
			SessionID: "sess-acp",
			AgentType: "hermes",
		}}},
	}

	sessions := col.collectSessions(context.Background(), runtime)
	if len(sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(sessions))
	}
	if sessions[0].SessionID != "sess-http" {
		t.Fatalf("session id = %q, want Hermes HTTP session", sessions[0].SessionID)
	}
	if sessions[0].TokenUsage != 42 {
		t.Fatalf("token usage = %d, want Hermes HTTP token usage", sessions[0].TokenUsage)
	}
}

func TestCollectSessionsFallsBackToACPWhenHermesHTTPFails(t *testing.T) {
	col := &Collector{}
	runtime := AgentRuntime{
		Agent:              store.CloudAgent{AgentID: "agent-1", AgentType: "hermes"},
		SupportsHermesHTTP: true,
		HermesClient:       fakeHermesSessions{err: errors.New("offline")},
		ACPSessionLister: fakeACPSessions{sessions: []model.SessionInfo{{
			SessionID: "sess-acp",
			AgentType: "hermes",
		}}},
	}

	sessions := col.collectSessions(context.Background(), runtime)
	if len(sessions) != 1 {
		t.Fatalf("session count = %d, want 1", len(sessions))
	}
	if sessions[0].SessionID != "sess-acp" {
		t.Fatalf("session id = %q, want ACP fallback session", sessions[0].SessionID)
	}
}

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
