package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
)

var ErrACPSessionBindingMissing = errors.New("manager session has no paxd native binding")

type ACPSessionBindingStore interface {
	NativeSessionID(ctx context.Context, connectionID string, managerSessionID string) (string, bool, error)
	ManagerSessionID(ctx context.Context, connectionID string, nativeSessionID string) (string, bool, error)
	BindSessionIDs(ctx context.Context, connectionID string, managerSessionID string, nativeSessionID string) error
}

type acpSessionBoundary struct {
	connectionID string
	store        ACPSessionBindingStore

	mu         sync.Mutex
	pendingNew map[string]string
}

func newACPSessionBoundary(connectionID string, store ACPSessionBindingStore) *acpSessionBoundary {
	return &acpSessionBoundary{
		connectionID: connectionID,
		store:        store,
		pendingNew:   make(map[string]string),
	}
}

func (b *acpSessionBoundary) inbound(
	ctx context.Context,
	managerSessionID string,
	nativeSessionHint string,
	payload []byte,
) (string, []byte, error) {
	msg, ok := parseACPRPCMessage(payload)
	if !ok {
		return "", payload, nil
	}
	if msg.Method == "session/new" {
		if managerSessionID != "" {
			b.mu.Lock()
			b.pendingNew[rpcIDKey(msg.ID)] = managerSessionID
			b.mu.Unlock()
		}
		return "", payload, nil
	}
	if managerSessionID == "" {
		return nativeSessionHint, payload, nil
	}

	nativeSessionID, found, err := b.nativeSessionID(ctx, managerSessionID)
	if err != nil {
		return "", nil, err
	}
	if !found && nativeSessionHint != "" {
		nativeSessionID = nativeSessionHint
		found = true
		_ = b.bind(ctx, managerSessionID, nativeSessionID)
	}
	if !found {
		return "", nil, ErrACPSessionBindingMissing
	}
	rewritten, _, err := rewriteACPSessionID(payload, nativeSessionID)
	return nativeSessionID, rewritten, err
}

func (b *acpSessionBoundary) outbound(
	ctx context.Context,
	nativeSessionID string,
	payload []byte,
) (string, []byte, error) {
	msg, ok := parseACPRPCMessage(payload)
	if !ok {
		return "", payload, nil
	}
	requestKey := rpcIDKey(msg.ID)
	b.mu.Lock()
	managerSessionID := b.pendingNew[requestKey]
	if managerSessionID != "" {
		delete(b.pendingNew, requestKey)
	}
	b.mu.Unlock()

	if managerSessionID != "" && msg.Error == nil {
		createdNativeID := nativeSessionID
		if createdNativeID == "" {
			createdNativeID = sessionIDFromResult(msg.Result)
		}
		if createdNativeID != "" {
			if err := b.bind(ctx, managerSessionID, createdNativeID); err != nil {
				return "", nil, err
			}
			nativeSessionID = createdNativeID
		}
	}
	if managerSessionID == "" && nativeSessionID != "" {
		var found bool
		var err error
		managerSessionID, found, err = b.managerSessionID(ctx, nativeSessionID)
		if err != nil {
			return "", nil, err
		}
		if !found {
			return "", payload, nil
		}
	}
	if managerSessionID == "" {
		return "", payload, nil
	}
	rewritten, _, err := rewriteACPSessionID(payload, managerSessionID)
	return managerSessionID, rewritten, err
}

func (b *acpSessionBoundary) nativeSessionID(ctx context.Context, managerSessionID string) (string, bool, error) {
	if b == nil || b.store == nil {
		return "", false, nil
	}
	return b.store.NativeSessionID(ctx, b.connectionID, managerSessionID)
}

func (b *acpSessionBoundary) managerSessionID(ctx context.Context, nativeSessionID string) (string, bool, error) {
	if b == nil || b.store == nil {
		return "", false, nil
	}
	return b.store.ManagerSessionID(ctx, b.connectionID, nativeSessionID)
}

func (b *acpSessionBoundary) bind(ctx context.Context, managerSessionID string, nativeSessionID string) error {
	if b == nil || b.store == nil || managerSessionID == "" || nativeSessionID == "" {
		return nil
	}
	return b.store.BindSessionIDs(ctx, b.connectionID, managerSessionID, nativeSessionID)
}

func rewriteACPSessionID(payload []byte, sessionID string) ([]byte, bool, error) {
	if sessionID == "" || !json.Valid(payload) {
		return payload, false, nil
	}
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		return nil, false, err
	}
	changed := false
	for _, field := range []string{"params", "result"} {
		container, ok := value[field].(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"sessionId", "session_id"} {
			current, ok := container[key]
			if !ok || current == sessionID {
				continue
			}
			container[key] = sessionID
			changed = true
		}
	}
	if !changed {
		return payload, false, nil
	}
	rewritten, err := json.Marshal(value)
	return rewritten, true, err
}
