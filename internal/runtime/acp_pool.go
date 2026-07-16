package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"github.com/pax-beehive/paxkit/reliablemq"
)

type ACPRouteStoreFactory interface {
	NewACPRouteStore(connectionID string) ACPRouteStore
}

type ACPRouteStoreFactoryFunc func(connectionID string) ACPRouteStore

func (f ACPRouteStoreFactoryFunc) NewACPRouteStore(connectionID string) ACPRouteStore {
	if f == nil {
		return nil
	}
	return f(connectionID)
}

type ACPPoolRegistry struct {
	mu          sync.Mutex
	routeStores ACPRouteStoreFactory
	pools       map[string]*ACPPool
}

func NewACPPoolRegistry(routeStores ACPRouteStoreFactory) *ACPPoolRegistry {
	return &ACPPoolRegistry{
		routeStores: routeStores,
		pools:       make(map[string]*ACPPool),
	}
}

func (r *ACPPoolRegistry) Get(connectionID string) (*ACPPool, error) {
	if r == nil {
		return nil, fmt.Errorf("acp pool registry is required")
	}
	if connectionID == "" {
		return nil, fmt.Errorf("connection id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if pool := r.pools[connectionID]; pool != nil {
		return pool, nil
	}
	if r.routeStores == nil {
		return nil, fmt.Errorf("acp route store factory is required")
	}
	routeStore := r.routeStores.NewACPRouteStore(connectionID)
	if routeStore == nil {
		return nil, fmt.Errorf("acp route store is required")
	}
	pool := NewACPPool(connectionID, routeStore)
	r.pools[connectionID] = pool
	return pool, nil
}

type ACPPool struct {
	connectionID string
	routeStore   ACPRouteStore
	router       *ACPRouter

	mu              sync.Mutex
	outputSink      ACPRouterOutputSink
	outputSinkToken uint64
}

func NewACPPool(connectionID string, routeStore ACPRouteStore) *ACPPool {
	pool := &ACPPool{
		connectionID: connectionID,
		routeStore:   routeStore,
	}
	pool.router = NewACPRouter(connectionID, routeStore, WithACPRouterOutputSink(ACPRouterOutputSinkFunc(func(ctx context.Context, nativeSessionID string, payload []byte) error {
		pool.mu.Lock()
		sink := pool.outputSink
		pool.mu.Unlock()
		if sink == nil {
			return fmt.Errorf("acp pool output sink is not attached")
		}
		return sink.EmitManagerFrame(ctx, nativeSessionID, payload)
	})))
	return pool
}

func (p *ACPPool) AttachOutputSink(sink ACPRouterOutputSink) func() {
	p.mu.Lock()
	p.outputSinkToken++
	token := p.outputSinkToken
	p.outputSink = sink
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		if p.outputSinkToken == token {
			p.outputSink = nil
		}
		p.mu.Unlock()
	}
}

func (p *ACPPool) UpsertSlot(slot ACPRouterSlot) {
	if p == nil || slot == nil {
		return
	}
	p.router.UpsertSlot(slot)
}

func (p *ACPPool) BeginSlotDrain(slotID string, processEpoch string) <-chan struct{} {
	if p == nil || p.router == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return p.router.BeginSlotDrain(slotID, processEpoch)
}

func (p *ACPPool) RemoveSlot(ctx context.Context, slotID string, processEpoch string) {
	if p == nil {
		return
	}
	p.router.RemoveSlot(slotID, processEpoch)
	if p.routeStore != nil && processEpoch != "" {
		cleared, err := p.routeStore.ClearACPSessionRoutesForProcess(ctx, p.connectionID, slotID, processEpoch)
		if err != nil {
			log.Printf("[paxd] acp pool connection_id=%s slot_id=%s process_epoch=%s route binding clear failed: %v", p.connectionID, slotID, processEpoch, err)
		} else if cleared > 0 {
			log.Printf("[paxd] acp pool connection_id=%s slot_id=%s process_epoch=%s cleared route bindings=%d", p.connectionID, slotID, processEpoch, cleared)
		}
	}
}

func (p *ACPPool) HandleManagerFrame(ctx context.Context, payload []byte) error {
	return p.HandleManagerFrameForSession(ctx, "", payload)
}

func (p *ACPPool) HandleManagerFrameForSession(ctx context.Context, nativeSessionID string, payload []byte) error {
	if p == nil {
		return fmt.Errorf("acp pool is required")
	}
	err := p.router.HandleManagerFrameForSession(ctx, nativeSessionID, payload)
	if err == nil {
		return nil
	}
	response, ok := acpRouterErrorResponse(payload, err)
	if !ok {
		return err
	}
	return p.router.output.EmitManagerFrame(ctx, nativeSessionID, response)
}

func acpRouterErrorResponse(payload []byte, err error) ([]byte, bool) {
	var routerErr ACPRouterError
	if !errors.As(err, &routerErr) {
		return nil, false
	}
	request, ok := parseACPRPCMessage(payload)
	if !ok || request.Method == "" || len(request.ID) == 0 {
		return nil, false
	}
	code := int64(-32000)
	data := map[string]any{"kind": routerErr.Code}
	switch routerErr.Code {
	case "slot_busy", "session_busy":
		code = -32001
		data["retryable"] = true
	case "session_route_missing":
		code = -32002
		data["requiresResume"] = true
	case "slot_unavailable", "slot_draining":
		code = -32003
		data["retryable"] = true
	}
	dataPayload, marshalErr := json.Marshal(data)
	if marshalErr != nil {
		return nil, false
	}
	response, marshalErr := json.Marshal(acpRPCMessage{
		JSONRPC: "2.0",
		ID:      append(json.RawMessage(nil), request.ID...),
		Error: &acpRPCError{
			Code:    code,
			Message: routerErr.Message,
			Data:    dataPayload,
		},
	})
	return response, marshalErr == nil
}

func (p *ACPPool) HandleSlotFrame(ctx context.Context, slotID string, processEpoch string, payload []byte) error {
	if p == nil {
		return fmt.Errorf("acp pool is required")
	}
	return p.router.HandleSlotFrame(ctx, slotID, processEpoch, payload)
}

func (p *ACPPool) OutputSinkForEngine(spec AgentConnectionSpec, engine ReliableEngine) ACPRouterOutputSink {
	return ACPRouterOutputSinkFunc(func(ctx context.Context, nativeSessionID string, payload []byte) error {
		metadata := reliablemq.Metadata{
			"agent_id": spec.CloudAgentID,
		}
		if nativeSessionID != "" {
			metadata["native_session_id"] = nativeSessionID
		}
		return engine.Send(ctx, reliablemq.OutboundMessage{
			QueueID:  spec.TransportQueueID,
			Stream:   reliablemq.StreamACP,
			Payload:  append([]byte(nil), payload...),
			Metadata: metadata,
		})
	})
}

type ACPSlotSessionConfig struct {
	Spec         ACPSlotSpec
	Runner       LocalACPProcessRunner
	Registry     *ACPPoolRegistry
	ReadyHandler func(context.Context, ACPSlotSpec)
}

type ACPSlotSession struct {
	cfg ACPSlotSessionConfig
}

func NewACPSlotSession(cfg ACPSlotSessionConfig) *ACPSlotSession {
	if cfg.Runner == nil {
		cfg.Runner = ExecLocalACPProcessRunner{}
	}
	return &ACPSlotSession{cfg: cfg}
}

func (s *ACPSlotSession) Run(ctx context.Context) Exit {
	if s.cfg.Registry == nil {
		return ConfigExit("missing_acp_pool_registry", "acp pool registry is required")
	}
	if s.cfg.Spec.ConnectionID == "" {
		return ConfigExit("missing_connection_id", "connection id is required")
	}
	if s.cfg.Spec.SlotID == "" {
		return ConfigExit("missing_slot_id", "slot id is required")
	}
	spec := s.cfg.Spec
	if spec.ProcessEpoch == "" {
		epoch, err := NewACPProcessEpoch()
		if err != nil {
			return TransientExit("process_epoch_failed", err.Error())
		}
		spec.ProcessEpoch = epoch
	}
	pool, err := s.cfg.Registry.Get(spec.ConnectionID)
	if err != nil {
		return ConfigExit("acp_pool_unavailable", err.Error())
	}
	var slot *ACPSlot
	slot = NewACPSlot(spec, s.cfg.Runner, WithACPSlotEventSink(ACPSlotEventSinkFunc(func(event ACPSlotEvent) {
		switch event.Type {
		case ACPSlotEventReady:
			pool.UpsertSlot(slot)
			if s.cfg.ReadyHandler != nil {
				s.cfg.ReadyHandler(context.Background(), spec)
			}
		case ACPSlotEventFrame:
			if err := pool.HandleSlotFrame(context.Background(), event.SlotID, event.ProcessEpoch, event.Payload); err != nil {
				log.Printf("[paxd] acp slot id=%s process_epoch=%s route frame failed: %v", event.SlotID, event.ProcessEpoch, err)
			}
		case ACPSlotEventTerminal:
			pool.RemoveSlot(context.Background(), event.SlotID, event.ProcessEpoch)
		}
	})))
	if err := slot.Start(ctx); err != nil {
		pool.RemoveSlot(context.Background(), spec.SlotID, spec.ProcessEpoch)
		return classifyProcessStartExit(err)
	}
	select {
	case <-slot.Done():
		return TransientExit("process_ended", "acp slot process ended")
	case <-ctx.Done():
		slot.Terminate(context.Background())
		return CanceledExit(ctx.Err())
	}
}

func NewACPProcessEpoch() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "epoch_" + hex.EncodeToString(random[:]), nil
}
