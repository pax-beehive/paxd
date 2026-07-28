package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/cloud"
)

func TestConversationAskTarget(t *testing.T) {
	t.Run("agent id yields an agent target", func(t *testing.T) {
		target, err := conversationAskTarget("agent_x", "", "")
		require.NoError(t, err)
		assert.Equal(t, conversationTargetKindAgent, target.Kind)
		assert.Equal(t, "agent_x", target.AgentID)
		assert.Empty(t, target.RepresentativeAgentID)
		assert.Empty(t, target.SessionID)
	})

	t.Run("agent id with session id targets that session", func(t *testing.T) {
		target, err := conversationAskTarget("agent_x", "", "sess_y")
		require.NoError(t, err)
		assert.Equal(t, conversationTargetKindAgent, target.Kind)
		assert.Equal(t, "agent_x", target.AgentID)
		assert.Equal(t, "sess_y", target.SessionID)
	})

	t.Run("representative id yields a representative target without session", func(t *testing.T) {
		target, err := conversationAskTarget("", "rep_x", "sess_y")
		require.NoError(t, err)
		assert.Equal(t, conversationTargetKindRepresentative, target.Kind)
		assert.Equal(t, "rep_x", target.RepresentativeAgentID)
		assert.Empty(t, target.AgentID)
		assert.Empty(t, target.SessionID)
	})

	t.Run("providing both is rejected", func(t *testing.T) {
		_, err := conversationAskTarget("agent_x", "rep_x", "")
		require.Error(t, err)
	})

	t.Run("providing neither is rejected", func(t *testing.T) {
		_, err := conversationAskTarget("", "", "")
		require.Error(t, err)
	})
}

func TestMCPIntArg(t *testing.T) {
	assert.Equal(t, 5, mcpIntArg(map[string]any{"limit": float64(5)}, "limit"))
	assert.Equal(t, 7, mcpIntArg(map[string]any{"limit": "7"}, "limit"))
	assert.Equal(t, 0, mcpIntArg(map[string]any{}, "limit"))
	assert.Equal(t, 0, mcpIntArg(map[string]any{"limit": nil}, "limit"))
}

func TestCallConversationMCPListAgents(t *testing.T) {
	t.Setenv(envPaxAgentID, "agent_caller")

	var gotParams cloud.ListOwnerAgentsParams
	original := listOwnerAgents
	listOwnerAgents = func(_ context.Context, params cloud.ListOwnerAgentsParams) ([]cloud.OwnerAgentView, error) {
		gotParams = params
		return []cloud.OwnerAgentView{{AgentID: "agent_target", Name: "Target", Status: "online"}}, nil
	}
	defer func() { listOwnerAgents = original }()

	result := callConversationMCPListAgents(context.Background(), map[string]any{
		"query":    "targ",
		"status":   "online",
		"order_by": "relevance",
		"limit":    float64(25),
	})
	require.False(t, result.IsError)
	require.Len(t, result.Content, 1)

	var agents []cloud.OwnerAgentView
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &agents))
	require.Len(t, agents, 1)
	assert.Equal(t, "agent_target", agents[0].AgentID)

	assert.Equal(t, "agent_caller", gotParams.FromAgentID)
	assert.Equal(t, "targ", gotParams.Query)
	assert.Equal(t, "online", gotParams.Status)
	assert.Equal(t, "relevance", gotParams.OrderBy)
	assert.Equal(t, 25, gotParams.Limit)
}

func TestCallConversationMCPListAgentsRequiresAgentID(t *testing.T) {
	t.Setenv(envPaxAgentID, "")
	result := callConversationMCPListAgents(context.Background(), map[string]any{})
	require.True(t, result.IsError)
}

func TestConversationMCPToolsExposesDiscoveryAndDualAsk(t *testing.T) {
	tools := conversationMCPTools()
	byName := make(map[string]map[string]any, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		byName[name] = tool
	}

	require.Contains(t, byName, "list_agents")

	ask, ok := byName["ask"]
	require.True(t, ok)
	schema, _ := ask["inputSchema"].(map[string]any)
	props, _ := schema["properties"].(map[string]any)
	require.Contains(t, props, "to_agent_id")
	require.Contains(t, props, "to_representative_agent_id")
	require.Contains(t, props, "to_session_id")
	// ask no longer forces a representative id: addressing is validated at call
	// time as an xor between to_agent_id and to_representative_agent_id.
	_, hasRequired := schema["required"]
	assert.False(t, hasRequired)
}
