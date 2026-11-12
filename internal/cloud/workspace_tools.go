package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// WorkspaceToolRequest identifies the caller's session, never a target owner or turn.
// Manager checks the active turn's authorization on every request.
type WorkspaceToolRequest struct {
	SessionID     string `json:"session_id"`
	Tool          string `json:"tool"`
	ArgumentsJSON string `json:"arguments_json"`
}

// WorkspaceToolFailure returns fixed safe messages without transport credentials.
func WorkspaceToolFailure(err error) (string, string) {
	return SecretaryToolFailure(err)
}

// WorkspaceTool uses node authentication without caching turn authorization.
// Access, status, and cancellation calls return immediately instead of waiting.
func (c *Client) WorkspaceTool(ctx context.Context, agentID string, request WorkspaceToolRequest) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		code, _ := WorkspaceToolFailure(err)
		return nil, secretaryFailure(code, err)
	}
	if agentID == "" || agentID == "." || agentID == ".." || strings.ContainsAny(agentID, "/\\?#%\r\n\t ") ||
		request.SessionID == "" || len(request.SessionID) > 4096 ||
		!validWorkspaceTool(request.Tool) {
		return nil, secretaryFailure("invalid_request", nil)
	}
	var arguments map[string]json.RawMessage
	if len(request.ArgumentsJSON) > secretaryToolMaxBytes || json.Unmarshal([]byte(request.ArgumentsJSON), &arguments) != nil || arguments == nil {
		return nil, secretaryFailure("invalid_request", nil)
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > secretaryToolMaxBytes {
		return nil, secretaryFailure("invalid_request", nil)
	}
	response, err := c.doWithContextAndHeaders(ctx, http.MethodPost, "/api/v1/node/agents/"+url.PathEscape(agentID)+"/workspace/tools", request, nil)
	if err != nil {
		code, _ := WorkspaceToolFailure(err)
		return nil, secretaryFailure(code, context.Cause(ctx))
	}
	return readWorkspaceToolResponse(ctx, response, request.Tool == "search")
}

func validWorkspaceTool(tool string) bool {
	switch tool {
	case "search", "request_access", "access_status", "cancel_access":
		return true
	default:
		return false
	}
}
