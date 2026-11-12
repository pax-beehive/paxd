package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/pax-beehive/paxd/internal/cloud"
)

func ordinaryWorkspaceMCPTools() []map[string]any {
	search := workspaceMCPTools()[0]
	search["description"] = "Search with an explicit scope. Default permission always covers only this agent's sessions: scope {\"agent_ids\":[\"self\"],\"session_ids\":[\"*\"]}, including under PAX auto approve. Other agents require an approved turn-scoped expansion. Declare the whole intended scope; an unauthorized scope is rejected, never silently narrowed or used to create an approval. Use workspace_request_access separately unless the user preauthorized the expansion. PAX auto approve approves explicit access requests; it never expands scope merely by starting a turn. Expanded authorization ends with the turn. The response includes the actual scope resolved by Manager."
	list := func(maximum int) workspaceSchema {
		value := workspaceArray(workspaceString(maximum), 50)
		value["minItems"] = 1
		return value
	}
	searchSchema := search["inputSchema"].(workspaceSchema)
	properties := searchSchema["properties"].(map[string]any)
	delete(properties, "session_ids")
	properties["scope"] = workspaceObject(map[string]any{
		"agent_ids": list(4096), "session_ids": list(4096),
	}, "agent_ids", "session_ids")
	searchSchema["required"] = []string{"query", "scope"}
	accessList := func(maximum int) workspaceSchema {
		value := list(maximum)
		value["default"] = []string{"*"}
		return value
	}
	timeout := workspaceInteger(0, 86400)
	timeout["default"] = 600
	return []map[string]any{
		search,
		{
			"name":        "workspace_request_access",
			"description": "Request a workspace search permission expansion separately from search. queries lists exact complete queries; agent_ids lists agents (self means this agent); session_ids lists sessions. Each defaults to [\"*\"]; * must be the only item. timeout_seconds is 0..86400, default 600; 0 means no time deadline, while the turn-end boundary still applies. PAX auto approve can approve explicit requests. This call is nonblocking. Preserve approval_id; while pending, continue other work and query workspace_access_status after poll_after_ms (normally 2000). Use workspace_cancel_access when abandoning the request. Approval does not automatically resume a finished agent; search separately after approval, within the same turn.",
			"inputSchema": workspaceObject(map[string]any{
				"queries": accessList(256), "agent_ids": accessList(4096), "session_ids": accessList(4096), "reason": workspaceString(4096),
				"timeout_seconds": timeout,
			}),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false},
		},
		{
			"name":        "workspace_access_status",
			"description": "Query a workspace access request by its returned approval_id. Returns pending, approved, denied, expired, or cancelled with the owning turn and scope. While pending, continue other work and query again after poll_after_ms (normally 2000). This call never waits, requests access, or resumes the agent automatically. Approval remains bound to its turn even when the request has no time deadline.",
			"inputSchema": workspaceObject(map[string]any{
				"approval_id": workspaceString(4096),
			}, "approval_id"),
			"annotations": map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		},
		{
			"name":        "workspace_cancel_access",
			"description": "Cancel this turn's workspace access request by approval_id when it is no longer needed. If it is already approved, also revoke that grant. Repeating cancellation is safe. Returns its latest status, including cancelled. This call never waits or resumes execution; cancellation does not authorize another turn.",
			"inputSchema": workspaceObject(map[string]any{
				"approval_id": workspaceString(4096),
			}, "approval_id"),
			"annotations": map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		},
	}
}

func isOrdinaryWorkspaceMCPTool(name string) bool {
	switch name {
	case "workspace_search", "workspace_request_access", "workspace_access_status", "workspace_cancel_access":
		return true
	default:
		return false
	}
}

var callOrdinaryWorkspace = func(ctx context.Context, agentID string, request cloud.WorkspaceToolRequest) (json.RawMessage, error) {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return nil, err
	}
	client, err := conversationCloudClient(ctx, cfg, agentID)
	if err != nil {
		return nil, err
	}
	return client.WorkspaceTool(ctx, agentID, request)
}

func callOrdinaryWorkspaceMCPTool(ctx context.Context, name string, raw json.RawMessage) (mcpToolCallResult, error) {
	args, err := parseWorkspaceArguments(name, raw, ordinaryWorkspaceMCPTools())
	if err != nil {
		if name == "workspace_search" {
			return mcpToolCallResult{}, ordinaryWorkspaceSearchInputError()
		}
		return mcpToolCallResult{}, err
	}
	switch name {
	case "workspace_request_access":
		for _, key := range []string{"queries", "agent_ids", "session_ids"} {
			if _, present := args[key]; !present {
				args[key] = []any{"*"}
			}
			if !validWorkspaceScopeList(args[key].([]any)) {
				return mcpToolCallResult{}, errors.New("invalid workspace access scope")
			}
		}
	case "workspace_search":
		scope := args["scope"].(map[string]any)
		if !validWorkspaceScopeList(scope["agent_ids"].([]any)) || !validWorkspaceScopeList(scope["session_ids"].([]any)) {
			return mcpToolCallResult{}, ordinaryWorkspaceSearchInputError()
		}
	case "workspace_access_status", "workspace_cancel_access":
		if strings.TrimSpace(args["approval_id"].(string)) == "" {
			return mcpToolCallResult{}, errors.New("approval_id must be nonempty")
		}
	}
	agentID, _, sessionID, err := conversationMCPIdentity()
	if err != nil {
		return workspaceMCPFailure(err), nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return mcpToolCallResult{}, errors.New("invalid workspace tool arguments")
	}
	data, err := callOrdinaryWorkspace(ctx, agentID, cloud.WorkspaceToolRequest{
		SessionID: sessionID, Tool: strings.TrimPrefix(name, "workspace_"), ArgumentsJSON: string(encoded),
	})
	if err != nil {
		return workspaceMCPFailure(err), nil
	}
	return workspaceMCPData(data)
}

func validWorkspaceScopeList(values []any) bool {
	for _, value := range values {
		text := strings.TrimSpace(value.(string))
		if text == "" || (text == "*" && len(values) != 1) {
			return false
		}
	}
	return true
}

func ordinaryWorkspaceSearchInputError() error {
	return errors.New(`workspace_search requires query and explicit scope, for example {"query":"cicd","scope":{"agent_ids":["self"],"session_ids":["*"]}}. Both scope lists must be nonempty; * must be the only item. Put session_ids inside scope. Other agents require workspace_request_access approval`)
}
