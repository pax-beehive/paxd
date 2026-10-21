package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type LocalSessionClient struct{ pool *ACPPool }
type localRequestContextKey struct{}
type LocalSession struct {
	NativeID string
	Result   json.RawMessage
}
type localCallResult struct {
	result json.RawMessage
	err    error
}

func NewLocalSessionClient(pool *ACPPool) *LocalSessionClient { return &LocalSessionClient{pool: pool} }

// WaitReady lets durable queued work survive the daemon's ACP startup window.
func (c *LocalSessionClient) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		c.pool.router.mu.Lock()
		ready := false
		for _, slot := range c.pool.router.slots {
			if slot.Ready() && !c.pool.router.slotDrainingLocked(slot) && c.pool.router.activeSlotPrompts[slot.SlotID()] == "" {
				ready = true
				break
			}
		}
		c.pool.router.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *LocalSessionClient) CreateSession(ctx context.Context, cwd string) (*LocalSession, error) {
	result, err := c.Request(ctx, "", "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return nil, err
	}
	id := sessionIDFromResult(result)
	if id == "" {
		return nil, errors.New("session/new returned no native session ID")
	}
	return &LocalSession{NativeID: id, Result: result}, nil
}

func (c *LocalSessionClient) ResumeSession(ctx context.Context, id, cwd string) error {
	// Persisted routes resume on demand during Prompt. Sending an extra resume
	// to a hot session can reset agent state and unnecessarily replay history.
	if _, exists, err := c.pool.routeStore.GetACPSessionRoute(ctx, c.pool.connectionID, id); err != nil {
		return err
	} else if exists {
		return nil
	}
	_, err := c.Request(ctx, id, "session/resume", map[string]any{"sessionId": id, "cwd": cwd, "mcpServers": []any{}})
	return err
}

func (c *LocalSessionClient) Prompt(ctx context.Context, id, text string, record func(context.Context, []byte) error) (json.RawMessage, error) {
	result, err := c.request(ctx, id, "session/prompt", map[string]any{"sessionId": id, "prompt": []any{map[string]string{"type": "text", "text": text}}}, record)
	if err != nil {
		return result, err
	}
	var completion struct {
		StopReason string `json:"stopReason"`
	}
	if err := json.Unmarshal(result, &completion); err != nil {
		return result, err
	}
	if completion.StopReason == "cancelled" {
		return result, errors.New("agent cancelled the scheduled prompt")
	}
	return result, nil
}

func (c *LocalSessionClient) Cancel(ctx context.Context, id string) error {
	data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]string{"sessionId": id}})
	return c.pool.HandleManagerFrameForSession(ctx, id, data)
}

func (c *LocalSessionClient) Request(ctx context.Context, id, method string, params any) (json.RawMessage, error) {
	return c.request(ctx, id, method, params, nil)
}

func (c *LocalSessionClient) request(ctx context.Context, sessionID, method string, params any, record func(context.Context, []byte) error) (json.RawMessage, error) {
	requestID, err := newE2EEEventID()
	if err != nil {
		return nil, err
	}
	requestID = "paxd.local." + requestID
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": requestID, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	keyBytes, _ := json.Marshal(requestID)
	key := rpcIDKey(keyBytes)
	ch := make(chan localCallResult, 1)
	c.pool.mu.Lock()
	if c.pool.localCalls == nil {
		c.pool.localCalls = make(map[string]chan localCallResult)
	}
	c.pool.localCalls[key] = ch
	c.pool.mu.Unlock()
	defer func() { c.pool.mu.Lock(); delete(c.pool.localCalls, key); c.pool.mu.Unlock() }()
	if record != nil {
		if err := record(ctx, data); err != nil {
			return nil, err
		}
	}
	dispatchCtx := context.WithValue(ctx, localRequestContextKey{}, sessionID)
	if err := c.pool.HandleManagerFrameForSession(dispatchCtx, sessionID, data); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		if method == "session/prompt" {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = c.Cancel(cancelCtx, sessionID)
			terminal, _ := json.Marshal(acpRPCMessage{JSONRPC: "2.0", ID: keyBytes, Error: &acpRPCError{Code: -32800, Message: ctx.Err().Error()}})
			_, _ = c.pool.deliverLocalFrame(cancelCtx, sessionID, terminal)
			cancel()
		}
		return nil, ctx.Err()
	}
}

func (p *ACPPool) failLocalSlot(ctx context.Context, slotID, epoch string) {
	requests := map[string]string{}
	p.router.mu.Lock()
	for key, pending := range p.router.pendingNew {
		if pending.slotID == slotID && pending.processEpoch == epoch {
			requests[key] = ""
		}
	}
	for key, pending := range p.router.pendingPrompts {
		if pending.slotID == slotID && pending.processEpoch == epoch {
			requests[key] = pending.nativeSessionID
		}
	}
	p.router.mu.Unlock()
	for key, sessionID := range requests {
		p.mu.Lock()
		call := p.localCalls[key]
		p.mu.Unlock()
		if call == nil {
			continue
		}
		// rpcIDKey preserves the JSON representation used for local string IDs.
		payload, err := json.Marshal(acpRPCMessage{JSONRPC: "2.0", ID: json.RawMessage(key), Error: &acpRPCError{Code: -32000, Message: "Agent process exited during local execution"}})
		if err == nil {
			_, _ = p.deliverLocalFrame(ctx, sessionID, payload)
		}
	}
}

// ObserveSession claims the output of a local execution without replacing the
// Manager transport sink. Client requests are journaled for encrypted delivery
// to the Console; unattended execution never grants permissions implicitly.
func (c *LocalSessionClient) ObserveSession(id string, observer func(context.Context, []byte) error) (func(), error) {
	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	if c.pool.localObservers == nil {
		c.pool.localObservers = make(map[string]func(context.Context, []byte) error)
	}
	if _, exists := c.pool.localObservers[id]; exists {
		return nil, errors.New("session already has a local execution")
	}
	c.pool.localObservers[id] = observer
	return func() { c.pool.mu.Lock(); delete(c.pool.localObservers, id); c.pool.mu.Unlock() }, nil
}

func (p *ACPPool) deliverLocalFrame(ctx context.Context, id string, data []byte) (bool, error) {
	msg, ok := parseACPRPCMessage(data)
	if !ok {
		return false, nil
	}
	p.mu.Lock()
	call := p.localCalls[rpcIDKey(msg.ID)]
	observer, observed := p.localObservers[id]
	p.mu.Unlock()
	p.router.mu.Lock()
	activeLocal := strings.Contains(p.router.activeSessionTurns[id], "paxd.local.")
	p.router.mu.Unlock()
	dispatchSession, localDispatch := ctx.Value(localRequestContextKey{}).(string)
	observed = observed && (activeLocal || call != nil || (localDispatch && dispatchSession == id))
	var recordErr error
	if observed && observer != nil {
		recordErr = observer(ctx, data)
	}
	if msg.Method == "" && call != nil {
		result := localCallResult{result: msg.Result, err: recordErr}
		if msg.Error != nil {
			result.err = fmt.Errorf("ACP error %d: %s", msg.Error.Code, msg.Error.Message)
		}
		select {
		case call <- result:
		default:
		}
		return true, recordErr
	}
	// Late local replies must not enter the Manager's unrelated request namespace.
	return observed || (msg.Method == "" && strings.Contains(string(msg.ID), "paxd.local.")), recordErr
}
