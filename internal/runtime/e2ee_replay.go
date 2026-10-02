package runtime

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/pax-beehive/paxkit/reliablemq"

	"github.com/pax-beehive/paxd/internal/e2ee"
)

// Record the original prompt, including encrypted attachment references, in the
// same reliable stream as replies. Replaying a turn does not require projections.
func (b *e2eeTransportBridge) sendReplayPrompt(ctx context.Context, session e2eeSessionContext, key, prompt []byte) error {
	turn := b.history.replayTurnID(session.sessionID)
	plaintext, err := json.Marshal(struct {
		TurnID string            `json:"turn_id"`
		Frames []json.RawMessage `json:"frames"`
	}{turn, []json.RawMessage{prompt}})
	if err != nil {
		return err
	}
	id, err := newE2EEEventID()
	if err != nil {
		return err
	}
	envelope, err := e2ee.Encrypt(key, e2ee.DirectionEvent, e2ee.Metadata{RecordID: id, AgentID: b.agentID, SessionID: session.sessionID, Kind: "acp_event", KeyEpoch: session.keyEpoch}, plaintext)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.send(ctx, payload, reliablemq.Metadata{"agent_id": b.agentID, "e2ee_kind": "event", "local_id": id, "turn_ref": turn, "connection_epoch": strconv.FormatInt(session.connectionEpoch, 10)})
}

func hasE2EEReplayPrompt(payload []byte) bool {
	var frame struct {
		Method string `json:"method"`
		Params struct {
			Prompt      []json.RawMessage `json:"prompt"`
			Attachments []json.RawMessage `json:"paxEncryptedAttachments"`
		} `json:"params"`
	}
	return json.Unmarshal(payload, &frame) == nil && frame.Method == "session/prompt" && (len(frame.Params.Prompt) > 0 || len(frame.Params.Attachments) > 0)
}
