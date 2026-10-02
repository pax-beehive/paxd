package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	e2eeHistoryPartBytes       = 256 * 1024
	e2eeHistoryCheckpointBytes = 16 * 1024
	e2eeHistoryCheckpointDelay = time.Second
)

type e2eeCanonicalRecord struct {
	kind      string
	messageID string
	partIndex int
	revision  int64
	plaintext []byte
}

type e2eeHistoryMessagePayload struct {
	MessageID   string          `json:"message_id"`
	Revision    int64           `json:"revision"`
	AgentID     string          `json:"agent_id"`
	SessionID   string          `json:"session_id"`
	Source      string          `json:"source"`
	Direction   string          `json:"direction"`
	Role        string          `json:"role,omitempty"`
	Status      string          `json:"status,omitempty"`
	MessageType string          `json:"message_type,omitempty"`
	TurnID      string          `json:"turn_id,omitempty"`
	ResponseID  string          `json:"response_id,omitempty"`
	RawJSON     json.RawMessage `json:"raw_json,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type e2eeHistoryPartPayload struct {
	MessageID   string          `json:"message_id"`
	PartIndex   int             `json:"part_index"`
	Revision    int64           `json:"revision"`
	PartType    string          `json:"part_type"`
	Text        string          `json:"text,omitempty"`
	PayloadJSON json.RawMessage `json:"payload_json,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type e2eeTextHistoryState struct {
	message      e2eeHistoryMessagePayload
	revision     int64
	text         string
	emittedBytes int
	partStart    int
	nextPart     int
	checkpointAt time.Time
}

type e2eeSessionHistoryState struct {
	turnByRequest map[string]string
	activeTurnID  string
	lastTurnID    string
	text          map[string]*e2eeTextHistoryState
	rawOrdinal    int64
	textOrdinal   int64
	activeTextKey string
}

type e2eeHistoryProjector struct {
	agentID string
	now     func() time.Time

	mu       sync.Mutex
	sessions map[string]*e2eeSessionHistoryState
}

func newE2EEHistoryProjector(agentID string, now func() time.Time) *e2eeHistoryProjector {
	if now == nil {
		now = time.Now
	}
	return &e2eeHistoryProjector{
		agentID:  agentID,
		now:      now,
		sessions: make(map[string]*e2eeSessionHistoryState),
	}
}

func (p *e2eeHistoryProjector) projectCommand(sessionID string, payload []byte) ([]e2eeCanonicalRecord, error) {
	msg, ok := parseACPRPCMessage(payload)
	if !ok || msg.Method != "session/prompt" {
		return nil, nil
	}
	requestKey := rpcIDKey(msg.ID)
	if requestKey == "" {
		return nil, nil
	}
	var params struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, err
	}
	var text strings.Builder
	hasAttachments := false
	for _, part := range params.Prompt {
		if part.Type == "resource_link" {
			hasAttachments = true
		}
		if part.Type == "" || part.Type == "text" {
			text.WriteString(part.Text)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.session(sessionID)
	turnID := e2eeStableID("turn", p.agentID, sessionID, requestKey)
	state.turnByRequest[requestKey] = turnID
	state.activeTurnID = turnID
	state.lastTurnID = turnID
	if text.Len() == 0 && !hasAttachments {
		return nil, nil
	}
	messageID := e2eeStableID("msg", p.agentID, sessionID, "user", requestKey)
	if _, exists := state.text["user:"+requestKey]; exists {
		return nil, nil
	}
	now := p.now().UTC()
	message := e2eeHistoryMessagePayload{
		MessageID: messageID, AgentID: p.agentID, SessionID: sessionID,
		Source: "e2ee_acp", Direction: "user_to_agent", Role: "user",
		Status: "complete", MessageType: "user_message", TurnID: turnID,
		CreatedAt: now, UpdatedAt: now,
	}
	if hasAttachments {
		message.RawJSON = append(json.RawMessage(nil), payload...)
	}
	state.text["user:"+requestKey] = &e2eeTextHistoryState{
		message: message, revision: 1, text: text.String(), emittedBytes: text.Len(),
	}
	return marshalE2EEHistoryRecords(message, 1, []e2eeHistoryPartPayload{{
		MessageID: messageID, PartIndex: 0, PartType: "text", Text: text.String(),
		CreatedAt: now, UpdatedAt: now,
	}})
}

func (p *e2eeHistoryProjector) projectFrames(
	sessionID string,
	frames []json.RawMessage,
) ([]e2eeCanonicalRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.session(sessionID)
	var records []e2eeCanonicalRecord
	for _, frame := range frames {
		msg, ok := parseACPRPCMessage(frame)
		if !ok {
			continue
		}
		updateType, content := e2eeHistoryUpdate(frame, msg)
		if updateType != "" && e2eeHistoryTextUpdate(updateType) && content != "" {
			projected, err := p.appendText(state, sessionID, updateType, content)
			if err != nil {
				return nil, err
			}
			records = append(records, projected...)
			continue
		}
		isResponse := len(msg.Result) > 0 || msg.Error != nil
		if msg.Method != "" || isResponse {
			finalized, err := p.checkpointText(state, true)
			if err != nil {
				return nil, err
			}
			records = append(records, finalized...)
		}
		if msg.Method == "session/update" && updateType != "" {
			raw, err := p.rawFrame(state, sessionID, updateType, frame)
			if err != nil {
				return nil, err
			}
			records = append(records, raw...)
			continue
		}
		if msg.Method != "" {
			raw, err := p.rawFrame(state, sessionID, msg.Method, frame)
			if err != nil {
				return nil, err
			}
			records = append(records, raw...)
			continue
		}
		requestKey := rpcIDKey(msg.ID)
		if requestKey != "" {
			if turnID := state.turnByRequest[requestKey]; turnID != "" {
				done, err := p.turnDone(state, sessionID, turnID, requestKey, frame)
				if err != nil {
					return nil, err
				}
				records = append(records, done...)
				delete(state.turnByRequest, requestKey)
				if state.activeTurnID == turnID {
					state.activeTurnID = ""
				}
			}
		}
	}
	return records, nil
}

func (p *e2eeHistoryProjector) activeTurnID(sessionID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session(sessionID).activeTurnID
}

// Late usage/status frames still belong to the most recently started turn.
func (p *e2eeHistoryProjector) replayTurnID(sessionID string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session(sessionID).lastTurnID
}

func (p *e2eeHistoryProjector) flushAll() (map[string][]e2eeCanonicalRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make(map[string][]e2eeCanonicalRecord)
	for sessionID, state := range p.sessions {
		finalized, err := p.checkpointText(state, true)
		if err != nil {
			return nil, err
		}
		result[sessionID] = finalized
	}
	return result, nil
}

func (p *e2eeHistoryProjector) appendText(
	state *e2eeSessionHistoryState,
	sessionID string,
	updateType string,
	delta string,
) ([]e2eeCanonicalRecord, error) {
	turnID := firstNonEmpty(state.activeTurnID, "turn_unbound")
	textState := state.text[state.activeTextKey]
	var records []e2eeCanonicalRecord
	if textState != nil && (textState.message.TurnID != turnID || textState.message.MessageType != updateType) {
		finalized, err := p.checkpointText(state, true)
		if err != nil {
			return nil, err
		}
		records = append(records, finalized...)
		textState = nil
	}
	if textState == nil {
		state.textOrdinal++
		key := turnID + "\x00" + updateType + "\x00" + fmt.Sprint(state.textOrdinal)
		now := p.now().UTC()
		messageID := e2eeStableID("msg", p.agentID, sessionID, turnID, updateType, "segment", fmt.Sprint(state.textOrdinal))
		textState = &e2eeTextHistoryState{
			message: e2eeHistoryMessagePayload{
				MessageID: messageID, AgentID: p.agentID, SessionID: sessionID,
				Source: "e2ee_acp", Direction: "agent_to_user", Role: "assistant",
				Status: "streaming", MessageType: updateType, TurnID: turnID,
				RawJSON:   json.RawMessage(`{"text_layout":"segment"}`),
				CreatedAt: now, UpdatedAt: now,
			},
			revision: 1, checkpointAt: now,
		}
		state.text[key] = textState
		state.activeTextKey = key
		header, err := marshalE2EEHistoryRecords(textState.message, textState.revision, nil)
		if err != nil {
			return nil, err
		}
		records = append(records, header...)
	}
	textState.text += delta
	for len(textState.text)-textState.partStart >= e2eeHistoryPartBytes {
		start := textState.partStart
		end := validUTF8Boundary(textState.text, start+e2eeHistoryPartBytes)
		partIndex := textState.nextPart
		textState.revision++
		now := p.now().UTC()
		partRecords, err := marshalE2EEHistoryRecords(textState.message, textState.revision,
			[]e2eeHistoryPartPayload{{
				MessageID: textState.message.MessageID, PartIndex: partIndex,
				PartType: e2eeHistoryPartType(updateType), Text: textState.text[start:end],
				CreatedAt: textState.message.CreatedAt, UpdatedAt: now,
			}})
		if err != nil {
			return nil, err
		}
		// The header was already emitted. Only the newly sealed part is needed.
		records = append(records, partRecords[1:]...)
		textState.emittedBytes = end
		textState.partStart = end
		textState.nextPart++
		textState.checkpointAt = now
	}
	if textState.emittedBytes < len(textState.text) {
		now := p.now().UTC()
		if len(textState.text)-textState.emittedBytes >= e2eeHistoryCheckpointBytes ||
			now.Sub(textState.checkpointAt) >= e2eeHistoryCheckpointDelay {
			textState.revision++
			part, err := marshalE2EEHistoryPart(e2eeHistoryPartPayload{
				MessageID: textState.message.MessageID, PartIndex: textState.nextPart,
				PartType:  e2eeHistoryPartType(textState.message.MessageType),
				Text:      textState.text[textState.partStart:],
				CreatedAt: textState.message.CreatedAt, UpdatedAt: now,
			}, textState.revision)
			if err != nil {
				return nil, err
			}
			records = append(records, part)
			textState.emittedBytes = len(textState.text)
			textState.checkpointAt = now
		}
	}
	return records, nil
}

func (p *e2eeHistoryProjector) checkpointText(
	state *e2eeSessionHistoryState,
	complete bool,
) ([]e2eeCanonicalRecord, error) {
	var records []e2eeCanonicalRecord
	for key, textState := range state.text {
		if strings.HasPrefix(key, "user:") || textState.message.Status == "complete" {
			continue
		}
		if !complete && textState.emittedBytes == len(textState.text) {
			continue
		}
		textState.revision++
		now := p.now().UTC()
		textState.message.UpdatedAt = now
		if complete {
			textState.message.Status = "complete"
			header, err := marshalE2EEHistoryRecords(textState.message, textState.revision, nil)
			if err != nil {
				return nil, err
			}
			records = append(records, header...)
		}
		if textState.emittedBytes < len(textState.text) {
			partIndex := textState.nextPart
			part, err := marshalE2EEHistoryPart(e2eeHistoryPartPayload{
				MessageID: textState.message.MessageID, PartIndex: partIndex,
				PartType: e2eeHistoryPartType(textState.message.MessageType),
				// An unsealed part is replaced at a higher revision. Sending the
				// whole part ensures an upsert never drops its earlier prefix.
				Text:      textState.text[textState.partStart:],
				CreatedAt: textState.message.CreatedAt, UpdatedAt: now,
			}, textState.revision)
			if err != nil {
				return nil, err
			}
			records = append(records, part)
			textState.emittedBytes = len(textState.text)
			textState.checkpointAt = now
		}
		if complete {
			delete(state.text, key)
			if state.activeTextKey == key {
				state.activeTextKey = ""
			}
		}
	}
	return records, nil
}

func (p *e2eeHistoryProjector) rawFrame(
	state *e2eeSessionHistoryState,
	sessionID string,
	messageType string,
	frame json.RawMessage,
) ([]e2eeCanonicalRecord, error) {
	state.rawOrdinal++
	now := p.now().UTC()
	messageID := e2eeStableID("msg", p.agentID, sessionID, state.activeTurnID,
		messageType, fmt.Sprint(state.rawOrdinal), string(frame))
	message := e2eeHistoryMessagePayload{
		MessageID: messageID, AgentID: p.agentID, SessionID: sessionID,
		Source: "e2ee_acp", Direction: "agent_to_user", Role: "assistant",
		Status: "complete", MessageType: messageType, TurnID: state.activeTurnID,
		RawJSON: append(json.RawMessage(nil), frame...), CreatedAt: now, UpdatedAt: now,
	}
	return marshalE2EEHistoryRecords(message, 1, []e2eeHistoryPartPayload{{
		MessageID: messageID, PartIndex: 0, PartType: "raw_json",
		PayloadJSON: append(json.RawMessage(nil), frame...), CreatedAt: now, UpdatedAt: now,
	}})
}

func (p *e2eeHistoryProjector) turnDone(
	_ *e2eeSessionHistoryState,
	sessionID string,
	turnID string,
	responseID string,
	frame json.RawMessage,
) ([]e2eeCanonicalRecord, error) {
	now := p.now().UTC()
	messageID := e2eeStableID("msg", p.agentID, sessionID, turnID, "turn_done")
	return marshalE2EEHistoryRecords(e2eeHistoryMessagePayload{
		MessageID: messageID, AgentID: p.agentID, SessionID: sessionID,
		Source: "e2ee_acp", Direction: "agent_to_user", Role: "assistant",
		Status: "complete", MessageType: "turn_done", TurnID: turnID,
		ResponseID: responseID, RawJSON: append(json.RawMessage(nil), frame...),
		CreatedAt: now, UpdatedAt: now,
	}, 1, nil)
}

func (p *e2eeHistoryProjector) session(sessionID string) *e2eeSessionHistoryState {
	state := p.sessions[sessionID]
	if state == nil {
		state = &e2eeSessionHistoryState{
			turnByRequest: make(map[string]string), text: make(map[string]*e2eeTextHistoryState),
		}
		p.sessions[sessionID] = state
	}
	return state
}

func marshalE2EEHistoryRecords(
	message e2eeHistoryMessagePayload,
	revision int64,
	parts []e2eeHistoryPartPayload,
) ([]e2eeCanonicalRecord, error) {
	message.Revision = revision
	payload, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	records := []e2eeCanonicalRecord{{
		kind: "e2ee_message", messageID: message.MessageID, partIndex: -1,
		revision: revision, plaintext: payload,
	}}
	for _, part := range parts {
		record, err := marshalE2EEHistoryPart(part, revision)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func marshalE2EEHistoryPart(part e2eeHistoryPartPayload, revision int64) (e2eeCanonicalRecord, error) {
	part.Revision = revision
	payload, err := json.Marshal(part)
	return e2eeCanonicalRecord{
		kind: "e2ee_message_part", messageID: part.MessageID,
		partIndex: part.PartIndex, revision: revision, plaintext: payload,
	}, err
}

func e2eeHistoryUpdate(payload []byte, msg acpRPCMessage) (string, string) {
	if msg.Method != "session/update" {
		return "", ""
	}
	var params struct {
		Update struct {
			Type    string `json:"sessionUpdate"`
			TypeAlt string `json:"session_update"`
			Content any    `json:"content"`
			Text    string `json:"text"`
			Delta   string `json:"delta"`
		} `json:"update"`
	}
	if json.Unmarshal(msg.Params, &params) != nil {
		return "", ""
	}
	content := firstNonEmpty(params.Update.Text, params.Update.Delta)
	switch value := params.Update.Content.(type) {
	case string:
		content = firstNonEmpty(content, value)
	case map[string]any:
		if text, ok := value["text"].(string); ok {
			content = firstNonEmpty(content, text)
		}
	}
	return firstNonEmpty(params.Update.Type, params.Update.TypeAlt), content
}

func e2eeHistoryTextUpdate(updateType string) bool {
	switch updateType {
	case "agent_message_chunk", "agent_message_delta", "message_delta",
		"agent_thought_chunk", "agent_thought_delta":
		return true
	default:
		return false
	}
}

func e2eeHistoryPartType(updateType string) string {
	if strings.Contains(updateType, "thought") {
		return "agent_thought_chunk"
	}
	return "text"
}

func validUTF8Boundary(value string, end int) int {
	if end >= len(value) {
		return len(value)
	}
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return end
}

func e2eeStableID(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + "_" + hex.EncodeToString(sum[:20])
}
