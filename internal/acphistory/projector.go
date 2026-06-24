package acphistory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pax-beehive/paxd/internal/daemonstore"
)

type Store interface {
	UpsertMessage(context.Context, *daemonstore.Message) error
	AppendMessagePartText(context.Context, string, int, string, string) error
}

const (
	messageSourceACP    = "acp_tunnel"
	messageDirectionOut = "paxd_to_manager"
)

type rpcMessage struct {
	ID     any             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type messageFields struct {
	SessionID     string
	TurnID        string
	ResponseID    string
	EntityType    string
	EventType     string
	SessionUpdate string
	Role          string
	Content       string
}

func ProjectOutbound(
	ctx context.Context,
	store Store,
	agentID string,
	seq int64,
	payload json.RawMessage,
) error {
	if store == nil {
		return nil
	}
	var rpc rpcMessage
	_ = json.Unmarshal(payload, &rpc)
	role := "assistant"
	fields := extractFields(payload, rpc)
	fields, ok := normalizeTextUpdate(rpc, fields)
	if !ok {
		return nil
	}
	if fields.Role != "" {
		role = fields.Role
	}
	messageType := firstNonEmpty(
		fields.SessionUpdate,
		strings.Trim(fields.EntityType+":"+fields.EventType, ":"),
		rpc.Method,
		"acp",
	)
	logicalKey := logicalKey(agentID, seq, fields)
	messageID := messageID(logicalKey)
	msg := daemonstore.Message{
		MessageID:   messageID,
		AgentID:     agentID,
		SessionID:   fields.SessionID,
		Source:      messageSourceACP,
		Direction:   messageDirectionOut,
		Role:        role,
		Status:      "received",
		MessageType: messageType,
		TurnID:      fields.TurnID,
		ResponseID:  fields.ResponseID,
		LogicalKey:  stringPtr(logicalKey),
	}
	if err := store.UpsertMessage(ctx, &msg); err != nil {
		return err
	}
	return store.AppendMessagePartText(ctx, msg.MessageID, 0, fields.Content, "")
}

func logicalKey(agentID string, seq int64, fields messageFields) string {
	if fields.SessionID != "" && fields.TurnID != "" {
		return fmt.Sprintf(
			"acp:%s:%s:%s:%s:%s:%s",
			agentID,
			messageDirectionOut,
			firstNonEmpty(fields.SessionID, "_"),
			fields.TurnID,
			firstNonEmpty(fields.SessionUpdate, "_"),
			firstNonEmpty(fields.Role, "_"),
		)
	}
	if fields.SessionID != "" {
		return fmt.Sprintf(
			"acp:%s:%s:%s:%s:%s",
			agentID,
			messageDirectionOut,
			fields.SessionID,
			firstNonEmpty(fields.SessionUpdate, "_"),
			firstNonEmpty(fields.Role, "_"),
		)
	}
	return fmt.Sprintf("acp:%s:%s:text:%d", agentID, messageDirectionOut, seq)
}

func messageID(logicalKey string) string {
	sum := sha256.Sum256([]byte(logicalKey))
	return fmt.Sprintf("msg_%x", sum[:24])
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func extractFields(payload json.RawMessage, rpc rpcMessage) messageFields {
	fields := fieldsFromRaw(payload)
	for _, raw := range []json.RawMessage{rpc.Params, rpc.Result} {
		nested := fieldsFromRaw(raw)
		fields.SessionID = firstNonEmpty(fields.SessionID, nested.SessionID)
		fields.TurnID = firstNonEmpty(fields.TurnID, nested.TurnID)
		fields.ResponseID = firstNonEmpty(fields.ResponseID, nested.ResponseID)
		fields.EntityType = firstNonEmpty(fields.EntityType, nested.EntityType)
		fields.EventType = firstNonEmpty(fields.EventType, nested.EventType)
		fields.SessionUpdate = firstNonEmpty(fields.SessionUpdate, nested.SessionUpdate)
		fields.Role = firstNonEmpty(fields.Role, nested.Role)
		fields.Content = firstNonEmpty(fields.Content, nested.Content)
	}
	return fields
}

func fieldsFromRaw(raw json.RawMessage) messageFields {
	if len(raw) == 0 {
		return messageFields{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return messageFields{}
	}
	obj, _ := v.(map[string]any)
	return messageFields{
		SessionID:     findString(obj, "sessionId", "session_id"),
		TurnID:        findString(obj, "turnId", "turn_id"),
		ResponseID:    findString(obj, "responseId", "response_id"),
		EntityType:    findString(obj, "entityType", "entity_type"),
		EventType:     findString(obj, "eventType", "event_type"),
		SessionUpdate: findString(obj, "sessionUpdate", "session_update"),
		Role:          findString(obj, "role"),
		Content:       findString(obj, "content", "text", "delta"),
	}
}

func findString(v any, keys ...string) string {
	switch typed := v.(type) {
	case map[string]any:
		for _, key := range keys {
			if val, ok := typed[key]; ok {
				if str, ok := val.(string); ok {
					return str
				}
			}
		}
		for _, val := range typed {
			if str := findString(val, keys...); str != "" {
				return str
			}
		}
	case []any:
		for _, val := range typed {
			if str := findString(val, keys...); str != "" {
				return str
			}
		}
	}
	return ""
}

func normalizeTextUpdate(rpc rpcMessage, fields messageFields) (messageFields, bool) {
	if fields.Content == "" {
		return fields, false
	}
	if strings.EqualFold(fields.EntityType, "message") &&
		strings.EqualFold(fields.EventType, "delta") {
		if fields.SessionUpdate == "" {
			fields.SessionUpdate = "message_delta"
		}
		return fields, true
	}
	return fields, rpc.Method == "session/update" && fields.SessionUpdate != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
