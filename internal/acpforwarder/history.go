package acpforwarder

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pax-beehive/paxd/internal/store"
)

type historyRPC struct {
	ID     any             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

type historyFields struct {
	SessionID  string
	TurnID     string
	ResponseID string
	EntityType string
	EventType  string
	Role       string
	Content    string
}

func projectTransportMessage(
	journal *store.Store,
	agentID string,
	stream string,
	seq int64,
	payload json.RawMessage,
) error {
	var rpc historyRPC
	_ = json.Unmarshal(payload, &rpc)
	direction := store.MessageDirectionPaxdToManager
	role := "assistant"
	localDirection := store.TransportDirectionOutbound
	if stream == store.TransportStreamManagerToPaxd {
		direction = store.MessageDirectionManagerToPaxd
		role = "user"
		localDirection = store.TransportDirectionInbound
	}
	fields := extractHistoryFields(payload, rpc)
	if fields.Role != "" {
		role = fields.Role
	}
	messageType := firstNonEmpty(
		strings.Trim(fields.EntityType+":"+fields.EventType, ":"),
		rpc.Method,
		"acp",
	)
	messageID := historyMessageID(agentID, stream, localDirection, seq, rpc, fields)
	msg := store.Message{
		MessageID:   messageID,
		AgentID:     agentID,
		SessionID:   fields.SessionID,
		Source:      store.MessageSourceACPTunnel,
		Direction:   direction,
		Role:        role,
		Status:      "received",
		MessageType: messageType,
		TurnID:      fields.TurnID,
		ResponseID:  fields.ResponseID,
		LogicalKey:  messageID,
		RawJSON:     string(payload),
	}
	if err := journal.UpsertMessage(&msg); err != nil {
		return err
	}
	if isTextDelta(fields) {
		return journal.AppendMessagePartText(msg.MessageID, 0, fields.Content, string(payload))
	}
	partType := store.MessagePartRawJSON
	text := ""
	if fields.Content != "" {
		partType = store.MessagePartText
		text = fields.Content
	}
	return journal.UpsertMessagePart(&store.MessagePart{
		MessageID:   msg.MessageID,
		PartIndex:   0,
		PartType:    partType,
		Text:        text,
		PayloadJSON: string(payload),
	})
}

func historyMessageID(
	agentID string,
	stream string,
	localDirection string,
	seq int64,
	rpc historyRPC,
	fields historyFields,
) string {
	if isTextDelta(fields) && fields.TurnID != "" {
		return fmt.Sprintf(
			"acp:%s:%s:%s:%s:%s",
			agentID,
			stream,
			firstNonEmpty(fields.SessionID, "_"),
			fields.TurnID,
			firstNonEmpty(fields.Role, "_"),
		)
	}
	if rpc.ID != nil {
		return fmt.Sprintf("acp:%s:%s:rpc:%v", agentID, localDirection, rpc.ID)
	}
	return fmt.Sprintf("acp:%s:%s:%d", agentID, localDirection, seq)
}

func extractHistoryFields(payload json.RawMessage, rpc historyRPC) historyFields {
	fields := fieldsFromRaw(payload)
	for _, raw := range []json.RawMessage{rpc.Params, rpc.Result} {
		nested := fieldsFromRaw(raw)
		fields.SessionID = firstNonEmpty(fields.SessionID, nested.SessionID)
		fields.TurnID = firstNonEmpty(fields.TurnID, nested.TurnID)
		fields.ResponseID = firstNonEmpty(fields.ResponseID, nested.ResponseID)
		fields.EntityType = firstNonEmpty(fields.EntityType, nested.EntityType)
		fields.EventType = firstNonEmpty(fields.EventType, nested.EventType)
		fields.Role = firstNonEmpty(fields.Role, nested.Role)
		fields.Content = firstNonEmpty(fields.Content, nested.Content)
	}
	return fields
}

func fieldsFromRaw(raw json.RawMessage) historyFields {
	if len(raw) == 0 {
		return historyFields{}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return historyFields{}
	}
	obj, _ := v.(map[string]any)
	return historyFields{
		SessionID:  findString(obj, "sessionId", "session_id"),
		TurnID:     findString(obj, "turnId", "turn_id"),
		ResponseID: findString(obj, "responseId", "response_id"),
		EntityType: findString(obj, "entityType", "entity_type"),
		EventType:  findString(obj, "eventType", "event_type"),
		Role:       findString(obj, "role"),
		Content:    findString(obj, "content", "text", "delta"),
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

func isTextDelta(fields historyFields) bool {
	return fields.Content != "" &&
		(strings.EqualFold(fields.EntityType, "message") &&
			strings.EqualFold(fields.EventType, "delta"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
