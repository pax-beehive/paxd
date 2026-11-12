package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const secretaryToolMaxBytes = 4 * 1024 * 1024

// SecretaryToolRequest carries an execution-scoped grant, never a user API key.
// SessionID identifies the calling execution; history's target stays inside ArgumentsJSON.
type SecretaryToolRequest struct {
	SessionID     string `json:"session_id"`
	Capability    string `json:"capability"`
	Tool          string `json:"tool"`
	ArgumentsJSON string `json:"arguments_json"`
}

// SecretaryToolError contains only a fixed safe code/message, not response bodies.
type SecretaryToolError struct {
	Code    string
	Message string
	cause   error
}

func (e *SecretaryToolError) Error() string { return e.Message }
func (e *SecretaryToolError) Unwrap() error { return e.cause }

func secretaryFailure(code string, cause error) *SecretaryToolError {
	messages := map[string]string{
		"invalid_request":        "The workspace tool request is invalid.",
		"unauthenticated":        "Manager authentication failed.",
		"authorization_required": "Workspace search scope is not authorized. Default permission covers all user-owned agents and sessions on this node (scope: {\"node_ids\":[\"self\"],\"agent_ids\":[\"*\"],\"session_ids\":[\"*\"]}), including under PAX auto approve. Other nodes require a turn-scoped expansion requested separately with workspace_request_access unless the user preauthorized it. PAX auto approve can approve that explicit request. Search never narrows the scope or requests access automatically.",
		"forbidden":              "Workspace access was denied or the execution is no longer active.",
		"not_found":              "The source was not found or is no longer accessible.",
		"version_unavailable":    "The referenced source version is unavailable; read the source again.",
		"version_mismatch":       "The source version changed; read the source again.",
		"version_conflict":       "The source version changed or is unavailable; read the source again.",
		"rate_limited":           "Manager rate limit reached; retry later.",
		"invalid_response":       "Manager returned an invalid workspace response.",
		"canceled":               "The workspace request was canceled.",
		"timeout":                "The workspace request timed out; retry later.",
		"unavailable":            "Workspace tools are unavailable; continue with the supplied context or retry later.",
	}
	message, ok := messages[code]
	if !ok {
		code = "unavailable"
		message = messages[code]
	}
	return &SecretaryToolError{Code: code, Message: message, cause: cause}
}

// SecretaryToolFailure turns arbitrary resolver/transport errors into safe MCP output.
func SecretaryToolFailure(err error) (string, string) {
	code := "unavailable"
	var failure *SecretaryToolError
	switch {
	case errors.Is(err, context.Canceled):
		code = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		code = "timeout"
	case errors.As(err, &failure):
		code = failure.Code
	}
	safe := secretaryFailure(code, nil)
	return safe.Code, safe.Message
}

// SecretaryTool uses existing node authentication and regional transport. It never
// follows redirects or includes credentials/arbitrary upstream bodies in errors.
func (c *Client) SecretaryTool(ctx context.Context, agentID string, request SecretaryToolRequest) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		code, _ := SecretaryToolFailure(err)
		return nil, secretaryFailure(code, err)
	}
	if agentID == "" || agentID == "." || agentID == ".." || strings.ContainsAny(agentID, "/\\?#%\r\n\t ") ||
		request.SessionID == "" || request.Capability == "" || len(request.Capability) > 16384 ||
		(request.Tool != "search" && request.Tool != "read" && request.Tool != "history" && request.Tool != "summaries") {
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
	response, err := c.doWithContextAndHeaders(ctx, http.MethodPost, "/api/v1/node/agents/"+url.PathEscape(agentID)+"/secretary/tools", request, nil)
	if err != nil {
		code, _ := SecretaryToolFailure(err)
		return nil, secretaryFailure(code, context.Cause(ctx))
	}
	return readWorkspaceToolResponse(ctx, response, false)
}

func readWorkspaceToolResponse(ctx context.Context, response *http.Response, searchAuthorization bool) (json.RawMessage, error) {
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, secretaryToolMaxBytes+1))
	if err != nil {
		code, _ := SecretaryToolFailure(err)
		return nil, secretaryFailure(code, context.Cause(ctx))
	}
	if len(raw) > secretaryToolMaxBytes {
		return nil, secretaryFailure("invalid_response", nil)
	}
	var envelope struct {
		Code    int             `json:"code"`
		Data    json.RawMessage `json:"data"`
		Message string          `json:"message"`
	}
	valid := json.Unmarshal(raw, &envelope) == nil
	status := response.StatusCode
	if status >= 200 && status < 300 {
		if !valid || envelope.Code < 100 || envelope.Code > 599 {
			return nil, secretaryFailure("invalid_response", nil)
		}
		if envelope.Code < 200 || envelope.Code >= 300 {
			status = envelope.Code
		}
	}
	if status < 200 || status >= 300 {
		if searchAuthorization && status == http.StatusForbidden {
			return nil, secretaryFailure("authorization_required", nil)
		}
		return nil, secretaryStatusFailure(status, envelope.Message)
	}
	if !valid || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, secretaryFailure("invalid_response", nil)
	}
	return envelope.Data, nil
}

func secretaryStatusFailure(status int, message string) error {
	if status == http.StatusConflict {
		if message == "source version unavailable" {
			return secretaryFailure("version_unavailable", nil)
		}
		if message == "summary version mismatch" {
			return secretaryFailure("version_mismatch", nil)
		}
	}
	codes := map[int]string{
		http.StatusBadRequest: "invalid_request", http.StatusUnauthorized: "unauthenticated",
		http.StatusForbidden: "forbidden", http.StatusNotFound: "unavailable",
		http.StatusConflict: "version_conflict", http.StatusTooManyRequests: "rate_limited",
	}
	return secretaryFailure(codes[status], nil)
}
