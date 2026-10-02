package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxkit/reliablemq"
)

const (
	e2eeBatchDelay = 75 * time.Millisecond
	e2eeBatchBytes = 16 * 1024
)

type e2eeSessionContext struct {
	sessionID       string
	keyEpoch        int64
	connectionEpoch int64
}

type e2eeOutputBatch struct {
	frames []json.RawMessage
	bytes  int
	timer  *time.Timer
}

type e2eePendingCanonical struct {
	payload  []byte
	metadata reliablemq.Metadata
}

type e2eeTransportBridge struct {
	rootKey         []byte
	rootKeyProvider e2ee.RootKeyProvider
	agentID         string
	queueID         string
	receipts        E2EECommandStore
	send            func(context.Context, []byte, reliablemq.Metadata) error
	history         *e2eeHistoryProjector

	mu             sync.Mutex
	sendMu         sync.Mutex
	flushMu        sync.Mutex
	closed         bool
	byNative       map[string]e2eeSessionContext
	bySession      map[string]e2eeSessionContext
	pending        map[string]e2eeSessionContext
	batches        map[string]*e2eeOutputBatch
	canonical      []e2eePendingCanonical
	retryCanonical *time.Timer
}

func newE2EETransportBridge(
	rootKey []byte,
	agentID string,
	queueID string,
	receipts E2EECommandStore,
	send func(context.Context, []byte, reliablemq.Metadata) error,
) *e2eeTransportBridge {
	return newE2EETransportBridgeWithProvider(rootKey, nil, agentID, queueID, receipts, send)
}

func newE2EETransportBridgeWithProvider(
	rootKey []byte,
	rootKeyProvider e2ee.RootKeyProvider,
	agentID string,
	queueID string,
	receipts E2EECommandStore,
	send func(context.Context, []byte, reliablemq.Metadata) error,
) *e2eeTransportBridge {
	if (len(rootKey) == 0 && rootKeyProvider == nil) || receipts == nil || send == nil {
		return nil
	}
	return &e2eeTransportBridge{
		rootKey: append([]byte(nil), rootKey...), rootKeyProvider: rootKeyProvider,
		agentID: agentID, queueID: queueID,
		receipts: receipts, send: send, byNative: make(map[string]e2eeSessionContext),
		bySession: make(map[string]e2eeSessionContext),
		pending:   make(map[string]e2eeSessionContext), batches: make(map[string]*e2eeOutputBatch),
		history: newE2EEHistoryProjector(agentID, time.Now),
	}
}

func (s *AgentTunnelSession) newE2EEBridge(engine ReliableEngine) *e2eeTransportBridge {
	return newE2EETransportBridgeWithProvider(
		s.deps.E2EERootKey,
		s.deps.E2EERootKeyProvider,
		s.spec.CloudAgentID,
		s.spec.TransportQueueID,
		s.deps.E2EECommandStore,
		func(ctx context.Context, payload []byte, metadata reliablemq.Metadata) error {
			return engine.Send(ctx, reliablemq.OutboundMessage{
				QueueID: s.spec.TransportQueueID, Stream: reliablemq.StreamACP,
				Payload: append([]byte(nil), payload...), Metadata: metadata,
			})
		},
	)
}

// Pool processes outlive individual WebSockets. Keep their encryption routes,
// pending batches, and history projector alive while the durable producer is
// disconnected, then rebind only the sender on the next tunnel attempt.
func (s *AgentTunnelSession) poolE2EEBridge(pool *ACPPool, engine ReliableEngine) *e2eeTransportBridge {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	candidate := s.newE2EEBridge(engine)
	if pool.e2eeBridge == nil || candidate == nil || candidate.agentID != pool.e2eeBridge.agentID {
		pool.e2eeBridge = candidate
		return candidate
	}
	bridge := pool.e2eeBridge
	bridge.sendMu.Lock()
	bridge.send = candidate.send
	bridge.sendMu.Unlock()
	return bridge
}

func (s *AgentTunnelSession) sendSessionOutput(
	ctx context.Context,
	managerSessionID string,
	nativeSessionID string,
	payload []byte,
	engine ReliableEngine,
	bridge *e2eeTransportBridge,
) error {
	if handled, err := bridge.sendOutput(ctx, nativeSessionID, payload); handled || err != nil {
		return err
	}
	metadata := reliablemq.Metadata{"agent_id": s.spec.CloudAgentID}
	if turnID := acpTurnID(ctx); turnID != "" {
		metadata["turn_id"] = turnID
	}
	if managerSessionID != "" {
		metadata["manager_session_id"] = managerSessionID
	}
	if nativeSessionID != "" {
		metadata["native_session_id"] = nativeSessionID
	}
	return engine.Send(ctx, reliablemq.OutboundMessage{
		QueueID: s.spec.TransportQueueID, Stream: reliablemq.StreamACP,
		Payload: append([]byte(nil), payload...), Metadata: metadata,
	})
}

func (b *e2eeTransportBridge) handleCommand(
	ctx context.Context,
	frame reliablemq.Frame,
	dispatch func(context.Context, string, []byte) (string, error),
) (bool, error) {
	if b == nil {
		var envelope e2ee.Envelope
		if json.Unmarshal(frame.Payload, &envelope) == nil && envelope.Kind == "acp_command" {
			return true, errors.New("E2EE command received but paxd has no root key")
		}
		return false, nil
	}
	var envelope e2ee.Envelope
	if err := json.Unmarshal(frame.Payload, &envelope); err != nil || envelope.ProtocolVersion == 0 {
		return false, nil
	}
	if envelope.Kind != "acp_command" || envelope.AgentID != b.agentID {
		return true, errors.New("invalid E2EE command route")
	}
	epoch, err := strconv.ParseInt(frame.Metadata["connection_epoch"], 10, 64)
	if err != nil || epoch < 1 {
		return true, errors.New("invalid E2EE connection epoch")
	}
	if commandID := frame.Metadata["command_id"]; commandID != "" && commandID != envelope.RecordID {
		return true, errors.New("E2EE command id metadata mismatch")
	}
	accepted, err := b.receipts.BeginE2EECommand(ctx, b.agentID, envelope.RecordID, epoch)
	if err != nil {
		return true, err
	}
	if !accepted {
		return true, b.sendCommandACK(ctx, envelope.RecordID, epoch)
	}
	rootKey, err := b.rootKeyForEpoch(ctx, envelope.KeyEpoch)
	if err != nil {
		return true, err
	}
	plaintext, err := e2ee.Decrypt(rootKey, e2ee.DirectionCommand, envelope)
	if err != nil {
		return true, err
	}
	session := e2eeSessionContext{
		sessionID: envelope.SessionID, keyEpoch: envelope.KeyEpoch, connectionEpoch: epoch,
	}
	b.trackCommand(plaintext, session)
	localized, err := localizeE2EEAttachments(ctx, plaintext, rootKey, envelope)
	if err != nil {
		var original struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(plaintext, &original)
		response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": original.ID, "error": map[string]any{"code": -32000, "message": err.Error()}})
		if _, sendErr := b.sendOutput(ctx, "", response); sendErr != nil {
			return true, sendErr
		}
		if completeErr := b.receipts.CompleteE2EECommand(ctx, b.agentID, envelope.RecordID); completeErr != nil {
			return true, completeErr
		}
		return true, b.sendCommandACK(ctx, envelope.RecordID, epoch)
	}
	originalPrompt := append([]byte(nil), plaintext...)
	plaintext = localized
	// Finish an earlier batch before assigning the next turn identity.
	if err := b.flush(ctx, session.sessionID); err != nil {
		return true, err
	}
	historyRecords, err := b.history.projectCommand(envelope.SessionID, plaintext)
	if err != nil {
		return true, err
	}
	if hasE2EEReplayPrompt(originalPrompt) {
		if err := b.sendReplayPrompt(ctx, session, rootKey, originalPrompt); err != nil {
			return true, err
		}
	}
	if err := b.enqueueCanonical(session, historyRecords); err != nil {
		return true, err
	}
	nativeSessionID, err := dispatch(ctx, envelope.SessionID, plaintext)
	if err != nil {
		return true, err
	}
	b.bindNativeSession(nativeSessionID, session)
	_ = b.flushCanonical(ctx)
	if err := b.receipts.CompleteE2EECommand(ctx, b.agentID, envelope.RecordID); err != nil {
		return true, err
	}
	return true, b.sendCommandACK(ctx, envelope.RecordID, epoch)
}

func (b *e2eeTransportBridge) sendOutput(ctx context.Context, nativeSessionID string, payload []byte) (bool, error) {
	if b == nil {
		return false, nil
	}
	session, ok := b.resolveOutput(nativeSessionID, payload)
	if !ok {
		return false, nil
	}
	frame := append(json.RawMessage(nil), payload...)
	boundary := e2eeBoundaryFrame(payload)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return true, errors.New("E2EE bridge is closed")
	}
	batch := b.batches[session.sessionID]
	if batch == nil {
		batch = &e2eeOutputBatch{}
		b.batches[session.sessionID] = batch
	}
	batch.frames = append(batch.frames, frame)
	batch.bytes += len(payload)
	if batch.timer == nil {
		batch.timer = time.AfterFunc(e2eeBatchDelay, func() {
			_ = b.flush(context.Background(), session.sessionID)
		})
	}
	flush := boundary || batch.bytes >= e2eeBatchBytes
	b.mu.Unlock()
	if flush {
		return true, b.flush(ctx, session.sessionID)
	}
	return true, nil
}

func (b *e2eeTransportBridge) close(ctx context.Context) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	b.closed = true
	if b.retryCanonical != nil {
		b.retryCanonical.Stop()
		b.retryCanonical = nil
	}
	sessions := make([]string, 0, len(b.batches))
	for sessionID := range b.batches {
		sessions = append(sessions, sessionID)
	}
	b.mu.Unlock()
	var result error
	for _, sessionID := range sessions {
		result = errors.Join(result, b.flush(ctx, sessionID))
	}
	if b.history != nil {
		recordsBySession, err := b.history.flushAll()
		result = errors.Join(result, err)
		for sessionID, records := range recordsBySession {
			b.mu.Lock()
			session, ok := b.bySession[sessionID]
			b.mu.Unlock()
			if ok {
				result = errors.Join(result, b.enqueueCanonical(session, records))
			}
		}
	}
	result = errors.Join(result, b.flushCanonical(ctx))
	return result
}

func (b *e2eeTransportBridge) flush(ctx context.Context, sessionID string) error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()
	b.mu.Lock()
	batch := b.batches[sessionID]
	if batch == nil || len(batch.frames) == 0 {
		b.mu.Unlock()
		return nil
	}
	delete(b.batches, sessionID)
	if batch.timer != nil {
		batch.timer.Stop()
	}
	session := b.bySession[sessionID]
	b.mu.Unlock()
	if session.keyEpoch == 0 {
		b.restoreBatch(sessionID, batch)
		return errors.New("missing E2EE session key epoch")
	}
	turnID := ""
	if b.history != nil {
		turnID = b.history.replayTurnID(sessionID)
	}
	plaintext, err := json.Marshal(struct {
		TurnID string            `json:"turn_id,omitempty"`
		Frames []json.RawMessage `json:"frames"`
	}{TurnID: turnID, Frames: batch.frames})
	if err != nil {
		b.restoreBatch(sessionID, batch)
		return err
	}
	recordID, err := newE2EEEventID()
	if err != nil {
		b.restoreBatch(sessionID, batch)
		return err
	}
	rootKey, err := b.rootKeyForEpoch(ctx, session.keyEpoch)
	if err != nil {
		b.restoreBatch(sessionID, batch)
		return err
	}
	envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionEvent, e2ee.Metadata{
		RecordID: recordID, AgentID: b.agentID, SessionID: sessionID,
		Kind: "acp_event", KeyEpoch: session.keyEpoch,
	}, plaintext)
	if err != nil {
		b.restoreBatch(sessionID, batch)
		return err
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		b.restoreBatch(sessionID, batch)
		return err
	}
	b.sendMu.Lock()
	err = b.send(ctx, payload, reliablemq.Metadata{
		"agent_id": b.agentID, "e2ee_kind": "event", "local_id": recordID, "turn_ref": turnID,
		"connection_epoch": strconv.FormatInt(session.connectionEpoch, 10),
	})
	b.sendMu.Unlock()
	if err != nil {
		b.restoreBatch(sessionID, batch)
		return err
	}
	if b.history != nil {
		records, projectErr := b.history.projectFrames(sessionID, batch.frames)
		if projectErr != nil {
			return projectErr
		}
		if enqueueErr := b.enqueueCanonical(session, records); enqueueErr != nil {
			return enqueueErr
		}
		_ = b.flushCanonical(ctx)
	}
	return nil
}

func (b *e2eeTransportBridge) enqueueCanonical(
	session e2eeSessionContext,
	records []e2eeCanonicalRecord,
) error {
	if b == nil || len(records) == 0 {
		return nil
	}
	pending := make([]e2eePendingCanonical, 0, len(records))
	for _, record := range records {
		recordID, err := newE2EEEventID()
		if err != nil {
			return err
		}
		rootKey, err := b.rootKeyForEpoch(context.Background(), session.keyEpoch)
		if err != nil {
			return err
		}
		envelope, err := e2ee.Encrypt(rootKey, e2ee.DirectionEvent, e2ee.Metadata{
			RecordID: recordID, AgentID: b.agentID, SessionID: session.sessionID,
			Kind: record.kind, KeyEpoch: session.keyEpoch,
		}, record.plaintext)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		metadata := reliablemq.Metadata{
			"agent_id": b.agentID, "e2ee_kind": "history", "local_id": recordID,
			"message_id":       record.messageID,
			"revision":         strconv.FormatInt(record.revision, 10),
			"connection_epoch": strconv.FormatInt(session.connectionEpoch, 10),
		}
		if record.partIndex >= 0 {
			metadata["part_index"] = strconv.Itoa(record.partIndex)
		}
		pending = append(pending, e2eePendingCanonical{payload: payload, metadata: metadata})
	}
	b.mu.Lock()
	b.canonical = append(b.canonical, pending...)
	b.mu.Unlock()
	return nil
}

func (b *e2eeTransportBridge) rootKeyForEpoch(
	ctx context.Context,
	keyEpoch int64,
) ([]byte, error) {
	if len(b.rootKey) > 0 {
		return append([]byte(nil), b.rootKey...), nil
	}
	if b.rootKeyProvider == nil {
		return nil, errors.New("paxd has no E2EE root key provider")
	}
	return b.rootKeyProvider.RootKey(ctx, b.agentID, keyEpoch)
}

func (b *e2eeTransportBridge) flushCanonical(ctx context.Context) error {
	if b == nil {
		return nil
	}
	for {
		b.mu.Lock()
		if len(b.canonical) == 0 {
			if b.retryCanonical != nil {
				b.retryCanonical.Stop()
				b.retryCanonical = nil
			}
			b.mu.Unlock()
			return nil
		}
		item := b.canonical[0]
		b.mu.Unlock()

		b.sendMu.Lock()
		err := b.send(ctx, item.payload, item.metadata)
		b.sendMu.Unlock()
		if err != nil {
			b.scheduleCanonicalRetry()
			return err
		}
		b.mu.Lock()
		if len(b.canonical) > 0 && string(b.canonical[0].payload) == string(item.payload) {
			b.canonical = b.canonical[1:]
		}
		b.mu.Unlock()
	}
}

func (b *e2eeTransportBridge) scheduleCanonicalRetry() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.retryCanonical != nil {
		return
	}
	b.retryCanonical = time.AfterFunc(5*time.Second, func() {
		b.mu.Lock()
		b.retryCanonical = nil
		b.mu.Unlock()
		_ = b.flushCanonical(context.Background())
	})
}

func (b *e2eeTransportBridge) restoreBatch(sessionID string, failed *e2eeOutputBatch) {
	b.mu.Lock()
	defer b.mu.Unlock()
	current := b.batches[sessionID]
	if current == nil {
		current = &e2eeOutputBatch{}
		b.batches[sessionID] = current
	}
	current.frames = append(append([]json.RawMessage(nil), failed.frames...), current.frames...)
	current.bytes += failed.bytes
	if !b.closed && current.timer == nil {
		current.timer = time.AfterFunc(e2eeBatchDelay, func() {
			_ = b.flush(context.Background(), sessionID)
		})
	}
}

func (b *e2eeTransportBridge) trackCommand(payload []byte, session e2eeSessionContext) {
	msg, ok := parseACPRPCMessage(payload)
	if !ok {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bySession[session.sessionID] = session
	if key := rpcIDKey(msg.ID); key != "" {
		b.pending[key] = session
	}
}

func (b *e2eeTransportBridge) bindNativeSession(nativeSessionID string, session e2eeSessionContext) {
	if nativeSessionID == "" {
		return
	}
	b.mu.Lock()
	b.byNative[nativeSessionID] = session
	b.mu.Unlock()
}

func (b *e2eeTransportBridge) resolveOutput(nativeSessionID string, payload []byte) (e2eeSessionContext, bool) {
	msg, ok := parseACPRPCMessage(payload)
	if !ok {
		return e2eeSessionContext{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if nativeSessionID != "" {
		if session, found := b.byNative[nativeSessionID]; found {
			return session, true
		}
	}
	if paramsSessionID := firstSessionID("", msg.Params); paramsSessionID != "" {
		if session, found := b.byNative[paramsSessionID]; found {
			return session, true
		}
	}
	key := rpcIDKey(msg.ID)
	session, found := b.pending[key]
	if !found {
		return e2eeSessionContext{}, false
	}
	delete(b.pending, key)
	if nativeSessionID != "" {
		b.byNative[nativeSessionID] = session
		return session, true
	}
	var result struct {
		SessionID    string `json:"sessionId"`
		SessionIDAlt string `json:"session_id"`
	}
	if len(msg.Result) > 0 && json.Unmarshal(msg.Result, &result) == nil {
		resolved := firstNonEmpty(result.SessionID, result.SessionIDAlt)
		if resolved != "" {
			b.byNative[resolved] = session
		}
	}
	return session, true
}

func (b *e2eeTransportBridge) sendCommandACK(ctx context.Context, commandID string, epoch int64) error {
	payload, err := json.Marshal(map[string]any{
		"type": "e2ee_command_ack", "command_id": commandID, "connection_epoch": epoch,
	})
	if err != nil {
		return err
	}
	b.sendMu.Lock()
	defer b.sendMu.Unlock()
	return b.send(ctx, payload, reliablemq.Metadata{
		"agent_id": b.agentID, "e2ee_kind": "command_ack", "command_id": commandID,
	})
}

func e2eeBoundaryFrame(payload []byte) bool {
	msg, ok := parseACPRPCMessage(payload)
	if !ok || msg.Method == "" {
		return true
	}
	if msg.Method != "session/update" {
		return true
	}
	var params struct {
		Update struct {
			Type    string `json:"sessionUpdate"`
			TypeAlt string `json:"session_update"`
		} `json:"update"`
	}
	if json.Unmarshal(msg.Params, &params) != nil {
		return true
	}
	switch firstNonEmpty(params.Update.Type, params.Update.TypeAlt) {
	case "agent_message_chunk", "agent_thought_chunk", "agent_message_delta", "agent_thought_delta":
		return false
	default:
		return true
	}
}

func newE2EEEventID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate E2EE event id: %w", err)
	}
	return "evt_" + hex.EncodeToString(value), nil
}
