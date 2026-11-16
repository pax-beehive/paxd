package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/pax-beehive/paxd/internal/cloud"
)

const envPaxSecretaryCapability = "PAX_SECRETARY_CAPABILITY"

type workspaceSchema = map[string]any

func workspaceString(maximum int) workspaceSchema {
	return workspaceSchema{"type": "string", "minLength": 1, "maxLength": maximum}
}
func workspaceInteger(minimum, maximum int64) workspaceSchema {
	return workspaceSchema{"type": "integer", "minimum": minimum, "maximum": maximum}
}
func workspaceEnum(values ...string) workspaceSchema {
	return workspaceSchema{"type": "string", "enum": values}
}
func workspaceObject(properties map[string]any, required ...string) workspaceSchema {
	schema := workspaceSchema{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
func workspaceArray(items workspaceSchema, maximum int) workspaceSchema {
	return workspaceSchema{"type": "array", "items": items, "maxItems": maximum}
}

func workspaceMCPTools() []map[string]any {
	summaryLimit := workspaceInteger(0, 50)
	summaryLimit["default"] = 20
	ref := workspaceObject(map[string]any{
		"kind": workspaceEnum("message", "session", "summary"), "id": workspaceString(4096),
		"revision": workspaceInteger(1, math.MaxInt64), "content_hash": workspaceString(256), "canonicalization_version": workspaceString(128),
	}, "kind", "id")
	tools := []map[string]any{
		{"name": "workspace_search", "description": "Find literal title/content matches in your current Manager messages and summaries. Read hits before quoting them.", "inputSchema": workspaceObject(map[string]any{
			"query": workspaceString(256), "kinds": workspaceArray(workspaceEnum("message", "session", "summary"), 3), "session_ids": workspaceArray(workspaceString(4096), 50), "cursor": workspaceString(16384), "limit": workspaceInteger(1, 50),
		}, "query")},
		{"name": "workspace_read", "description": "Read a source or exact summary revision. An omitted message view uses the Manager's compact role-aware default; request full for more visible text and page as needed. Preserve returned refs, versions, views and read receipts. An omitted summary revision reads the current version.", "inputSchema": workspaceObject(map[string]any{
			"ref": ref, "view": workspaceEnum("last", "first", "full"), "offset": workspaceInteger(0, 1000000000), "limit": workspaceInteger(0, 16384), "before": workspaceInteger(0, 50), "after": workspaceInteger(0, 50),
		}, "ref")},
		{"name": "workspace_expand", "description": "Read first/full visible text of one message at its exact version, excluding tools and reasoning. Preserve returned refs and read receipts.", "inputSchema": workspaceObject(map[string]any{
			"message_id": workspaceString(4096), "content_hash": workspaceString(256), "canonicalization_version": workspaceString(128), "view": workspaceEnum("first", "full"), "offset": workspaceInteger(0, 1000000000), "limit": workspaceInteger(0, 16384),
		}, "message_id", "content_hash", "canonicalization_version", "view")},
		{"name": "workspace_history", "description": "Read a bounded page of visible messages from a session, optionally scoped to turn_id. An omitted view uses the Manager's compact role-aware default; set view=full to inspect a turn's full visible text and page as needed. Use read/expand for messages with more text. Tools and reasoning are excluded. Use at most one nonzero before_seq or after_seq. Preserve exact refs and read receipts; pagination is not a processing checkpoint.", "inputSchema": workspaceObject(map[string]any{
			"session_id": workspaceString(4096), "turn_id": workspaceString(4096), "view": workspaceEnum("first", "last", "full"), "before_seq": workspaceInteger(0, math.MaxInt64), "after_seq": workspaceInteger(0, math.MaxInt64), "limit": workspaceInteger(0, 50),
		}, "session_id")},
		{"name": "workspace_summaries", "description": "List a bounded page of existing summary Nodes organized in the current source session. Follow next_cursor when more remain; an empty prompt cursor with summaries_has_more means start at the first page. Items normally contain a complete summary revision. An oversized item is only a summary_id/revision/title header with requires_read:true, not a read of its body or sources. Before modifying it, call workspace_read with ref {kind:\"summary\",id:summary_id,revision:revision} for the complete exact version. Summary reads share the task read budget and fail explicitly before returning if it is exceeded. The Manager may return fewer than limit to fit the page byte budget. The limit defaults to 20 when omitted or zero, with a maximum of 50. This cursor is not a processing checkpoint.", "inputSchema": workspaceObject(map[string]any{
			"session_id": workspaceString(4096), "cursor": workspaceString(16384), "limit": summaryLimit,
		}, "session_id")},
	}
	for _, tool := range tools {
		tool["annotations"] = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	}
	return tools
}

func isWorkspaceMCPTool(name string) bool {
	switch name {
	case "workspace_search", "workspace_read", "workspace_expand", "workspace_history", "workspace_summaries":
		return true
	default:
		return false
	}
}

var callSecretaryWorkspace = func(ctx context.Context, agentID string, request cloud.SecretaryToolRequest) (json.RawMessage, error) {
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return nil, err
	}
	client, err := conversationCloudClient(ctx, cfg, agentID)
	if err != nil {
		return nil, err
	}
	return client.SecretaryTool(ctx, agentID, request)
}

func callWorkspaceMCPTool(ctx context.Context, name string, raw json.RawMessage) (mcpToolCallResult, error) {
	args, err := parseWorkspaceArguments(name, raw, workspaceMCPTools())
	if err != nil {
		return mcpToolCallResult{}, err
	}
	if name == "workspace_history" && workspacePositive(args["before_seq"]) && workspacePositive(args["after_seq"]) {
		return mcpToolCallResult{}, errors.New("history accepts only one pagination direction")
	}
	tool := strings.TrimPrefix(name, "workspace_")
	if name == "workspace_expand" {
		args["ref"] = map[string]any{"kind": "message", "id": args["message_id"], "content_hash": args["content_hash"], "canonicalization_version": args["canonicalization_version"]}
		delete(args, "message_id")
		delete(args, "content_hash")
		delete(args, "canonicalization_version")
		tool = "read"
	}
	agentID, _, sessionID, err := conversationMCPIdentity()
	if err != nil {
		return workspaceMCPFailure(err), nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return mcpToolCallResult{}, errors.New("invalid workspace tool arguments")
	}
	data, err := callSecretaryWorkspace(ctx, agentID, cloud.SecretaryToolRequest{SessionID: sessionID, Capability: os.Getenv(envPaxSecretaryCapability), Tool: tool, ArgumentsJSON: string(encoded)})
	if err != nil {
		return workspaceMCPFailure(err), nil
	}
	return workspaceMCPData(data)
}

func parseWorkspaceArguments(name string, raw json.RawMessage, tools []map[string]any) (map[string]any, error) {
	var arguments any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if len(raw) > 4*1024*1024 || decoder.Decode(&arguments) != nil {
		return nil, errors.New("invalid workspace tool arguments")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("invalid workspace tool arguments")
	}
	var schema workspaceSchema
	for _, tool := range tools {
		if tool["name"] == name {
			schema = tool["inputSchema"].(workspaceSchema)
			break
		}
	}
	if schema == nil || !validWorkspaceValue(arguments, schema) {
		return nil, errors.New("invalid workspace tool arguments")
	}
	return arguments.(map[string]any), nil
}

func workspaceMCPData(data json.RawMessage) (mcpToolCallResult, error) {
	if !json.Valid(data) {
		return workspaceMCPFailure(errors.New("invalid workspace response")), nil
	}
	var object map[string]json.RawMessage
	var structured any = data
	if json.Unmarshal(data, &object) != nil || object == nil {
		structured = map[string]any{"data": data}
	}
	return mcpToolCallResult{Content: []mcpToolContent{{Type: "text", Text: string(data)}}, StructuredContent: structured}, nil
}

func workspaceMCPFailure(err error) mcpToolCallResult {
	code, message := cloud.SecretaryToolFailure(err)
	return mcpToolCallResult{IsError: true, Content: []mcpToolContent{{Type: "text", Text: message}}, StructuredContent: map[string]any{"error": map[string]string{"code": code, "message": message}}}
}

func workspacePositive(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	n, err := number.Int64()
	return err == nil && n > 0
}

// Validate the advertised closed schemas before crossing the node-auth boundary.
func validWorkspaceValue(value any, schema workspaceSchema) bool {
	switch schema["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return false
		}
		properties := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]string)
		for _, key := range required {
			if _, ok = object[key]; !ok {
				return false
			}
		}
		for key, child := range object {
			property, known := properties[key]
			if !known || !validWorkspaceValue(child, property.(workspaceSchema)) {
				return false
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok || len(array) > schema["maxItems"].(int) {
			return false
		}
		if minimum, bounded := schema["minItems"].(int); bounded && len(array) < minimum {
			return false
		}
		for _, child := range array {
			if !validWorkspaceValue(child, schema["items"].(workspaceSchema)) {
				return false
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return false
		}
		if values, enumerated := schema["enum"].([]string); enumerated {
			for _, allowed := range values {
				if text == allowed {
					return true
				}
			}
			return false
		}
		length := utf8.RuneCountInString(text)
		return length >= schema["minLength"].(int) && length <= schema["maxLength"].(int)
	case "integer":
		number, ok := value.(json.Number)
		if !ok {
			return false
		}
		integer, err := strconv.ParseInt(number.String(), 10, 64)
		return err == nil && integer >= schema["minimum"].(int64) && integer <= schema["maximum"].(int64)
	default:
		return false
	}
	return true
}
