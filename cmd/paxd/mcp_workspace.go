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
	return workspaceSchema{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func workspaceArray(items workspaceSchema, maximum int) workspaceSchema {
	return workspaceSchema{"type": "array", "items": items, "maxItems": maximum}
}

func workspaceMCPTools() []map[string]any {
	ref := workspaceObject(map[string]any{
		"kind": workspaceEnum("message", "session", "summary"), "id": workspaceString(4096),
		"revision": workspaceInteger(1, math.MaxInt64), "content_hash": workspaceString(256), "canonicalization_version": workspaceString(128),
	}, "kind", "id")
	tools := []map[string]any{
		{"name": "workspace_search", "description": "Find literal title/content matches in your current Manager messages and summaries. Read hits before quoting them.", "inputSchema": workspaceObject(map[string]any{
			"query": workspaceString(256), "kinds": workspaceArray(workspaceEnum("message", "session", "summary"), 3), "session_ids": workspaceArray(workspaceString(4096), 50), "cursor": workspaceString(16384), "limit": workspaceInteger(1, 50),
		}, "query")},
		{"name": "workspace_read", "description": "Read a source or exact summary revision. Preserve returned refs, versions, views and read receipts. An omitted summary revision reads the current version.", "inputSchema": workspaceObject(map[string]any{
			"ref": ref, "view": workspaceEnum("last", "first", "full"), "offset": workspaceInteger(0, 1000000000), "limit": workspaceInteger(0, 16384), "before": workspaceInteger(0, 50), "after": workspaceInteger(0, 50),
		}, "ref")},
		{"name": "workspace_expand", "description": "Read first/full visible text of one message at its exact version, excluding tools and reasoning. Preserve returned refs and read receipts.", "inputSchema": workspaceObject(map[string]any{
			"message_id": workspaceString(4096), "content_hash": workspaceString(256), "canonicalization_version": workspaceString(128), "view": workspaceEnum("first", "full"), "offset": workspaceInteger(0, 1000000000), "limit": workspaceInteger(0, 16384),
		}, "message_id", "content_hash", "canonicalization_version", "view")},
		{"name": "workspace_history", "description": "Read a bounded page of canonical visible messages from a workspace session. Use at most one nonzero before_seq or after_seq. Preserve each exact message ref; pagination is not a processing checkpoint.", "inputSchema": workspaceObject(map[string]any{
			"session_id": workspaceString(4096), "before_seq": workspaceInteger(0, math.MaxInt64), "after_seq": workspaceInteger(0, math.MaxInt64), "limit": workspaceInteger(0, 50),
		}, "session_id")},
	}
	for _, tool := range tools {
		tool["annotations"] = map[string]any{"readOnlyHint": true, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false}
	}
	return tools
}

func isWorkspaceMCPTool(name string) bool {
	switch name {
	case "workspace_search", "workspace_read", "workspace_expand", "workspace_history":
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
	var arguments any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if len(raw) > 4*1024*1024 || decoder.Decode(&arguments) != nil {
		return mcpToolCallResult{}, errors.New("invalid workspace tool arguments")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return mcpToolCallResult{}, errors.New("invalid workspace tool arguments")
	}
	var schema workspaceSchema
	for _, tool := range workspaceMCPTools() {
		if tool["name"] == name {
			schema = tool["inputSchema"].(workspaceSchema)
			break
		}
	}
	if schema == nil || !validWorkspaceValue(arguments, schema) {
		return mcpToolCallResult{}, errors.New("invalid workspace tool arguments")
	}
	args := arguments.(map[string]any)
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
		for _, key := range schema["required"].([]string) {
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
