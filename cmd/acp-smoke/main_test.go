package main

import "testing"

func TestParseUserAgentsEnvelope(t *testing.T) {
	got, err := parseUserAgents([]byte(`{
		"data": {
			"agents": [
				{"agent_id":"agent_1","name":"codex","agent_type":"codex","status":"online","online":true}
			]
		},
		"code": 200,
		"message": "ok"
	}`))
	if err != nil {
		t.Fatalf("parseUserAgents() error = %v", err)
	}
	if len(got) != 1 || got[0].AgentID != "agent_1" || !got[0].Online {
		t.Fatalf("parseUserAgents() = %#v", got)
	}
}

func TestChooseAgentAutoSelectsSingleOnlineAgent(t *testing.T) {
	got, err := chooseAgent([]userAgent{
		{AgentID: "agent_offline", Name: "offline", Status: "offline"},
		{AgentID: "agent_codex", Name: "codex", AgentType: "codex", Online: true},
	}, "", false)
	if err != nil {
		t.Fatalf("chooseAgent() error = %v", err)
	}
	if got.AgentID != "agent_codex" {
		t.Fatalf("chooseAgent() = %#v", got)
	}
}

func TestChooseAgentMatchesSelector(t *testing.T) {
	got, err := chooseAgent([]userAgent{
		{AgentID: "agent_codex", Name: "work", AgentType: "codex", Online: true},
		{AgentID: "agent_claude", Name: "review", AgentType: "claude-code", Online: true},
	}, "review", false)
	if err != nil {
		t.Fatalf("chooseAgent() error = %v", err)
	}
	if got.AgentID != "agent_claude" {
		t.Fatalf("chooseAgent() = %#v", got)
	}
}

func TestChooseAgentRejectsMultipleOnlineAgents(t *testing.T) {
	_, err := chooseAgent([]userAgent{
		{AgentID: "agent_codex", Online: true},
		{AgentID: "agent_claude", Online: true},
	}, "", false)
	if err == nil {
		t.Fatal("chooseAgent() error = nil, want multiple agents error")
	}
}
