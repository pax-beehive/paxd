package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxkit/reliablemq"
)

type LocalHistoryFrame struct {
	ID        uint64
	Direction string
	Payload   json.RawMessage
	CreatedAt time.Time
}

// PublishLocalHistory reuses canonical encrypted message projection. A durable
// caller cursor advances only after every envelope has entered reliable transport.
// Snapshot revisions are monotonic even when a live transcript is replayed.
func (p *ACPPool) PublishLocalHistory(ctx context.Context, agentID, sessionID, runID string, frames []LocalHistoryFrame, after uint64, keys e2ee.RootKeyProvider, persist func(context.Context, string, []byte) ([]byte, error)) error {
	if len(frames) == 0 {
		return nil
	}
	p.mu.Lock()
	bridge := p.e2eeBridge
	p.mu.Unlock()
	if bridge == nil {
		return errors.New("session history transport is not connected")
	}
	key, err := keys.RootKey(ctx, agentID, 1)
	if err != nil {
		return err
	}
	projector := newE2EEHistoryProjector(agentID, func() time.Time { return frames[0].CreatedAt })
	var records []e2eeCanonicalRecord
	var output []LocalHistoryFrame
	for _, frame := range frames {
		payload, _, err := rewriteACPSessionID(frame.Payload, sessionID)
		if err != nil {
			return err
		}
		var projected []e2eeCanonicalRecord
		if frame.Direction == "input" {
			projected, err = projector.projectCommand(sessionID, payload)
		} else {
			projected, err = projector.projectFrames(sessionID, []json.RawMessage{payload})
			if frame.ID > after {
				frame.Payload = payload
				output = append(output, frame)
			}
		}
		if err != nil {
			return err
		}
		records = append(records, projected...)
	}
	projector.mu.Lock()
	tail, err := projector.checkpointText(projector.session(sessionID), false)
	projector.mu.Unlock()
	if err != nil {
		return err
	}
	records = append(records, tail...)
	snapshot := frames[len(frames)-1].ID
	for _, record := range records {
		record.revision += int64(snapshot) * 1000000
		if record.kind == "e2ee_message" || record.kind == "e2ee_message_part" {
			var object map[string]any
			if err := json.Unmarshal(record.plaintext, &object); err != nil {
				return err
			}
			object["revision"] = record.revision
			record.plaintext, err = json.Marshal(object)
			if err != nil {
				return err
			}
		}
		id := e2eeStableID("cron_history", runID, record.messageID, strconv.Itoa(record.partIndex), strconv.FormatInt(record.revision, 10))
		metadata := reliablemq.Metadata{"agent_id": agentID, "e2ee_kind": "history", "local_id": id, "message_id": record.messageID, "revision": strconv.FormatInt(record.revision, 10)}
		if record.partIndex >= 0 {
			metadata["part_index"] = strconv.Itoa(record.partIndex)
		}
		if err := sendLocalEncrypted(ctx, bridge, key, agentID, sessionID, id, record.kind, record.plaintext, metadata, persist); err != nil {
			return err
		}
	}
	// Publish live frames as well, so permission requests and updates reach open consoles.
	for _, frame := range output {
		plaintext, _ := json.Marshal(map[string]any{"frames": []json.RawMessage{frame.Payload}})
		id := e2eeStableID("cron_event", runID, strconv.FormatUint(frame.ID, 10))
		if err := sendLocalEncrypted(ctx, bridge, key, agentID, sessionID, id, "acp_event", plaintext, reliablemq.Metadata{"agent_id": agentID, "e2ee_kind": "event", "local_id": id}, persist); err != nil {
			return err
		}
	}
	return nil
}

func sendLocalEncrypted(ctx context.Context, bridge *e2eeTransportBridge, key []byte, agentID, sessionID, id, kind string, plaintext []byte, metadata reliablemq.Metadata, persist func(context.Context, string, []byte) ([]byte, error)) error {
	envelope, err := e2ee.Encrypt(key, e2ee.DirectionEvent, e2ee.Metadata{RecordID: id, AgentID: agentID, SessionID: sessionID, Kind: kind, KeyEpoch: 1}, plaintext)
	if err != nil {
		return err
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	data, err = persist(ctx, id, data)
	if err != nil {
		return err
	}
	bridge.sendMu.Lock()
	defer bridge.sendMu.Unlock()
	return bridge.send(ctx, data, metadata)
}
