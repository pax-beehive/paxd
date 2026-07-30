package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	internalACPResumeIDPrefix          = "paxd.resume."
	paxConversationMCPName             = "pax-conversation"
	paxConversationMCPNameSuffixLength = 8
)

type ACPRouterError struct {
	Code    string
	Message string
}

func (e ACPRouterError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

type ACPRoute struct {
	ConnectionID      string
	NativeSessionID   string
	BoundSlotID       string
	BoundProcessEpoch string
	LastSlotID        string
	ResumeParams      json.RawMessage
	Version           int64
}

type ACPRouteBindingUpdate struct {
	ConnectionID    string
	NativeSessionID string
	SlotID          string
	ProcessEpoch    string
	ExpectedVersion int64
	ResumeParams    json.RawMessage
}

type ACPRouteStore interface {
	GetACPSessionRoute(ctx context.Context, connectionID string, nativeSessionID string) (ACPRoute, bool, error)
	UpsertACPSessionRoute(ctx context.Context, connectionID string, nativeSessionID string, resumeParams json.RawMessage) (ACPRoute, error)
	BindACPSessionRoute(ctx context.Context, update ACPRouteBindingUpdate) (ACPRoute, bool, error)
	CountBoundACPSessionRoutesBySlot(ctx context.Context, connectionID string) (map[string]int, error)
	ClearACPSessionRoutesForProcess(ctx context.Context, connectionID string, slotID string, processEpoch string) (int64, error)
}

type ACPRouterSlot interface {
	SlotID() string
	ProcessEpoch() string
	Ordinal() int
	Ready() bool
	Send(ctx context.Context, payload []byte) error
}

type ACPRouterOutputSink interface {
	EmitManagerFrame(ctx context.Context, nativeSessionID string, payload []byte) error
}

type ACPRouterOutputSinkFunc func(ctx context.Context, nativeSessionID string, payload []byte) error

func (f ACPRouterOutputSinkFunc) EmitManagerFrame(ctx context.Context, nativeSessionID string, payload []byte) error {
	if f == nil {
		return nil
	}
	return f(ctx, nativeSessionID, payload)
}

type ACPRouter struct {
	connectionID string
	store        ACPRouteStore
	output       ACPRouterOutputSink
	executable   func() (string, error)
	workspace    func(string) (string, error)

	mu                 sync.Mutex
	slots              map[string]ACPRouterSlot
	pendingNew         map[string]pendingNewSession
	pendingPrompts     map[string]pendingPrompt
	pendingWorkerReqs  map[string]pendingWorkerRequest
	activeSlotPrompts  map[string]string
	activeSessionTurns map[string]string
	waiters            map[string]chan acpRPCWaitResult
	slotReservations   map[string]int
	lastPrompt         map[string]uint64
	lastAssigned       map[string]uint64
	drainingSlots      map[string]*slotDrain
	promptSeq          uint64
	assignmentSeq      uint64
	resumeSeq          int64
}

type ACPRouterOption func(*ACPRouter)

func WithACPRouterOutputSink(sink ACPRouterOutputSink) ACPRouterOption {
	return func(router *ACPRouter) {
		if sink != nil {
			router.output = sink
		}
	}
}

func WithACPRouterExecutablePath(resolver func() (string, error)) ACPRouterOption {
	return func(router *ACPRouter) {
		if resolver != nil {
			router.executable = resolver
		}
	}
}

func WithACPRouterWorkspaceResolver(
	resolver func(string) (string, error),
) ACPRouterOption {
	return func(router *ACPRouter) {
		if resolver != nil {
			router.workspace = resolver
		}
	}
}

type pendingNewSession struct {
	slotID       string
	processEpoch string
	resumeParams json.RawMessage
}

type pendingPrompt struct {
	nativeSessionID string
	slotID          string
	processEpoch    string
}

type pendingWorkerRequest struct {
	slotID       string
	processEpoch string
}

type slotDrain struct {
	processEpoch string
	done         chan struct{}
	closed       bool
}

func NewACPRouter(connectionID string, store ACPRouteStore, opts ...ACPRouterOption) *ACPRouter {
	router := &ACPRouter{
		connectionID:       connectionID,
		store:              store,
		output:             ACPRouterOutputSinkFunc(nil),
		executable:         os.Executable,
		workspace:          resolveSessionWorkspace,
		slots:              make(map[string]ACPRouterSlot),
		pendingNew:         make(map[string]pendingNewSession),
		pendingPrompts:     make(map[string]pendingPrompt),
		pendingWorkerReqs:  make(map[string]pendingWorkerRequest),
		activeSlotPrompts:  make(map[string]string),
		activeSessionTurns: make(map[string]string),
		waiters:            make(map[string]chan acpRPCWaitResult),
		slotReservations:   make(map[string]int),
		lastPrompt:         make(map[string]uint64),
		lastAssigned:       make(map[string]uint64),
		drainingSlots:      make(map[string]*slotDrain),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(router)
		}
	}
	return router
}

func (r *ACPRouter) UpsertSlot(slot ACPRouterSlot) {
	if slot == nil {
		return
	}
	r.mu.Lock()
	if drain := r.drainingSlots[slot.SlotID()]; drain != nil && drain.processEpoch != slot.ProcessEpoch() {
		r.closeSlotDrainLocked(drain)
		delete(r.drainingSlots, slot.SlotID())
	}
	r.slots[slot.SlotID()] = slot
	r.mu.Unlock()
}

func (r *ACPRouter) BeginSlotDrain(slotID string, processEpoch string) <-chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[slotID]
	if slot == nil || (processEpoch != "" && slot.ProcessEpoch() != processEpoch) {
		done := make(chan struct{})
		close(done)
		return done
	}
	if drain := r.drainingSlots[slotID]; drain != nil && drain.processEpoch == slot.ProcessEpoch() {
		return drain.done
	}
	drain := &slotDrain{processEpoch: slot.ProcessEpoch(), done: make(chan struct{})}
	r.drainingSlots[slotID] = drain
	r.signalSlotDrainIfIdleLocked(slotID)
	return drain.done
}

func (r *ACPRouter) RemoveSlot(slotID string, processEpoch string) {
	r.mu.Lock()
	if slot := r.slots[slotID]; slot != nil && (processEpoch == "" || slot.ProcessEpoch() == processEpoch) {
		delete(r.slots, slotID)
	}
	if drain := r.drainingSlots[slotID]; drain != nil && (processEpoch == "" || drain.processEpoch == processEpoch) {
		r.closeSlotDrainLocked(drain)
		delete(r.drainingSlots, slotID)
	}
	waiterPrefix := slotID + "|"
	if processEpoch != "" {
		waiterPrefix += processEpoch + "|"
	}
	for key, waiter := range r.waiters {
		if strings.HasPrefix(key, waiterPrefix) {
			delete(r.waiters, key)
			waiter <- acpRPCWaitResult{err: fmt.Errorf("slot process exited")}
		}
	}
	for id, prompt := range r.pendingPrompts {
		if prompt.slotID == slotID && prompt.processEpoch == processEpoch {
			delete(r.pendingPrompts, id)
			delete(r.activeSessionTurns, prompt.nativeSessionID)
			delete(r.activeSlotPrompts, slotID)
		}
	}
	for id, pending := range r.pendingNew {
		if pending.slotID == slotID && (processEpoch == "" || pending.processEpoch == processEpoch) {
			delete(r.pendingNew, id)
			if r.slotReservations[slotID] > 0 {
				r.slotReservations[slotID]--
			}
		}
	}
	r.mu.Unlock()
}

func (r *ACPRouter) HandleManagerFrame(ctx context.Context, payload []byte) error {
	return r.HandleManagerFrameForSession(ctx, "", payload)
}

func (r *ACPRouter) HandleManagerFrameForSession(ctx context.Context, nativeSessionID string, payload []byte) error {
	msg, ok := parseACPRPCMessage(payload)
	if !ok {
		return ACPRouterError{Code: "invalid_acp_frame", Message: "payload must be a JSON-RPC object"}
	}
	if msg.Method == "" {
		return r.handleManagerResponse(ctx, nativeSessionID, msg, payload)
	}
	switch msg.Method {
	case "session/new":
		localizedMsg, localizedPayload, err := r.localizeSessionLifecycleMessage(msg)
		if err != nil {
			return err
		}
		return r.handleSessionNew(ctx, localizedMsg, localizedPayload)
	case "session/resume":
		localizedMsg, localizedPayload, err := r.localizeSessionLifecycleMessage(msg)
		if err != nil {
			return err
		}
		sessionID := firstSessionID(nativeSessionID, localizedMsg.Params)
		return r.handleExplicitSessionResume(ctx, sessionID, localizedMsg, localizedPayload)
	case "session/prompt":
		sessionID := firstSessionID(nativeSessionID, msg.Params)
		return r.handleSessionOperation(ctx, sessionID, msg, payload, true)
	case "session/cancel":
		sessionID := firstSessionID(nativeSessionID, msg.Params)
		return r.handleSessionOperation(ctx, sessionID, msg, payload, false)
	default:
		sessionID := firstSessionID(nativeSessionID, msg.Params)
		if sessionID == "" {
			return ACPRouterError{Code: "session_id_required", Message: "manager frame has no native session id"}
		}
		return r.handleSessionOperation(ctx, sessionID, msg, payload, false)
	}
}

func (r *ACPRouter) handleExplicitSessionResume(
	ctx context.Context,
	nativeSessionID string,
	msg acpRPCMessage,
	payload []byte,
) error {
	if nativeSessionID == "" {
		return ACPRouterError{Code: "session_id_required", Message: "manager frame has no native session id"}
	}
	if rpcIDKey(msg.ID) == "" {
		return ACPRouterError{Code: "request_id_required", Message: "session/resume requires an id"}
	}
	if _, ok, err := r.store.GetACPSessionRoute(ctx, r.connectionID, nativeSessionID); err != nil {
		return err
	} else if ok {
		return r.handleSessionOperation(ctx, nativeSessionID, msg, payload, false)
	}
	resumeParams, err := resumeDescriptorFromSessionNewParams(msg.Params)
	if err != nil {
		return err
	}
	route, err := r.store.UpsertACPSessionRoute(ctx, r.connectionID, nativeSessionID, resumeParams)
	if err != nil {
		return err
	}
	slot, err := r.selectReadySlot(ctx)
	if err != nil {
		return err
	}
	defer r.releaseSlotReservation(slot.SlotID())
	if err := r.resumeColdRoute(ctx, slot, route); err != nil {
		return err
	}
	response, err := json.Marshal(acpRPCMessage{
		JSONRPC: "2.0",
		ID:      append(json.RawMessage(nil), msg.ID...),
		Result:  json.RawMessage(`{}`),
	})
	if err != nil {
		return err
	}
	return r.output.EmitManagerFrame(ctx, nativeSessionID, response)
}

func (r *ACPRouter) HandleSlotFrame(ctx context.Context, slotID string, processEpoch string, payload []byte) error {
	msg, ok := parseACPRPCMessage(payload)
	if !ok {
		return ACPRouterError{Code: "invalid_acp_frame", Message: "payload must be a JSON-RPC object"}
	}
	if r.resolveInternalWaiter(slotID, processEpoch, msg) {
		return nil
	}
	if msg.Method == "" {
		nativeSessionID, err := r.handleSessionNewResponse(ctx, slotID, processEpoch, msg, payload)
		if err != nil {
			return err
		}
		if promptSessionID := r.releasePromptForResponse(msg.ID, slotID, processEpoch); nativeSessionID == "" {
			nativeSessionID = promptSessionID
		}
		return r.output.EmitManagerFrame(ctx, nativeSessionID, append([]byte(nil), payload...))
	}
	nativeSessionID := firstSessionID("", msg.Params)
	if len(bytes.TrimSpace(msg.ID)) > 0 {
		if nativeSessionID != "" {
			key := workerRequestKey(nativeSessionID, msg.ID)
			r.mu.Lock()
			r.pendingWorkerReqs[key] = pendingWorkerRequest{slotID: slotID, processEpoch: processEpoch}
			r.mu.Unlock()
		}
	}
	return r.output.EmitManagerFrame(ctx, nativeSessionID, append([]byte(nil), payload...))
}

func (r *ACPRouter) handleSessionNew(ctx context.Context, msg acpRPCMessage, payload []byte) error {
	resumeParams, err := resumeDescriptorFromSessionNewParams(msg.Params)
	if err != nil {
		return err
	}
	key := rpcIDKey(msg.ID)
	if key == "" {
		return ACPRouterError{Code: "request_id_required", Message: "session/new requires an id"}
	}
	slot, err := r.selectReadySlot(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.pendingNew[key] = pendingNewSession{
		slotID:       slot.SlotID(),
		processEpoch: slot.ProcessEpoch(),
		resumeParams: resumeParams,
	}
	r.mu.Unlock()
	if err := slot.Send(ctx, append([]byte(nil), payload...)); err != nil {
		r.mu.Lock()
		delete(r.pendingNew, key)
		r.mu.Unlock()
		r.releaseSlotReservation(slot.SlotID())
		return err
	}
	return nil
}

func (r *ACPRouter) handleSessionOperation(ctx context.Context, nativeSessionID string, msg acpRPCMessage, payload []byte, needsPromptLease bool) error {
	if nativeSessionID == "" {
		return ACPRouterError{Code: "session_id_required", Message: "manager frame has no native session id"}
	}
	route, ok, err := r.store.GetACPSessionRoute(ctx, r.connectionID, nativeSessionID)
	if err != nil {
		return err
	}
	if !ok {
		return ACPRouterError{Code: "session_route_missing", Message: "native session has no paxd route"}
	}
	allowDraining := msg.Method == "session/cancel"
	slot, hot := r.hotSlot(route, allowDraining)
	if !hot && !allowDraining && r.routeSlotDraining(route) {
		return ACPRouterError{Code: "slot_draining", Message: "ACP slot is draining"}
	}
	reserved := false
	if !hot {
		slot, err = r.selectReadySlot(ctx)
		if err != nil {
			return err
		}
		reserved = true
		if err := r.resumeColdRoute(ctx, slot, route); err != nil {
			r.releaseSlotReservation(slot.SlotID())
			return err
		}
	}
	if needsPromptLease {
		if err := r.acquirePromptLease(nativeSessionID, slot, msg.ID); err != nil {
			if reserved {
				r.releaseSlotReservation(slot.SlotID())
			}
			return err
		}
	}
	if err := slot.Send(ctx, append([]byte(nil), payload...)); err != nil {
		if reserved {
			r.releaseSlotReservation(slot.SlotID())
		}
		if needsPromptLease {
			r.releasePrompt(nativeSessionID, slot.SlotID(), rpcIDKey(msg.ID))
		}
		return err
	}
	if needsPromptLease {
		r.recordPromptAccepted(slot.SlotID())
	}
	if reserved {
		r.releaseSlotReservation(slot.SlotID())
	}
	return nil
}

func (r *ACPRouter) handleManagerResponse(ctx context.Context, nativeSessionID string, msg acpRPCMessage, payload []byte) error {
	if nativeSessionID == "" {
		return ACPRouterError{Code: "session_id_required", Message: "manager response requires native session context"}
	}
	key := workerRequestKey(nativeSessionID, msg.ID)
	r.mu.Lock()
	source, ok := r.pendingWorkerReqs[key]
	if ok {
		delete(r.pendingWorkerReqs, key)
	}
	slot := r.slots[source.slotID]
	live := slot != nil && slot.ProcessEpoch() == source.processEpoch && slot.Ready()
	r.mu.Unlock()
	if !ok {
		return ACPRouterError{Code: "worker_request_missing", Message: "no pending worker request for response"}
	}
	if !live {
		r.notifySlotDrain(source.slotID)
		return ACPRouterError{Code: "worker_request_source_stale", Message: "worker request source is no longer live"}
	}
	err := slot.Send(ctx, append([]byte(nil), payload...))
	r.notifySlotDrain(source.slotID)
	return err
}

func (r *ACPRouter) handleSessionNewResponse(ctx context.Context, slotID string, processEpoch string, msg acpRPCMessage, payload []byte) (string, error) {
	key := rpcIDKey(msg.ID)
	r.mu.Lock()
	pending, ok := r.pendingNew[key]
	if ok && (pending.slotID != slotID || pending.processEpoch != processEpoch) {
		r.mu.Unlock()
		return "", ACPRouterError{Code: "session_new_source_mismatch", Message: "session/new response came from a different slot epoch"}
	}
	if ok {
		delete(r.pendingNew, key)
	}
	r.mu.Unlock()
	if ok {
		r.releaseSlotReservation(pending.slotID)
	}
	if !ok || msg.Error != nil {
		return "", nil
	}
	sessionID := sessionIDFromResult(msg.Result)
	if sessionID == "" {
		return "", ACPRouterError{Code: "session_id_required", Message: "session/new result has no session id"}
	}
	resumeParams := append(json.RawMessage(nil), pending.resumeParams...)
	route, err := r.store.UpsertACPSessionRoute(ctx, r.connectionID, sessionID, resumeParams)
	if err != nil {
		return "", err
	}
	_, bound, err := r.store.BindACPSessionRoute(ctx, ACPRouteBindingUpdate{
		ConnectionID:    r.connectionID,
		NativeSessionID: sessionID,
		SlotID:          slotID,
		ProcessEpoch:    processEpoch,
		ExpectedVersion: route.Version,
		ResumeParams:    resumeParams,
	})
	if err != nil {
		return "", err
	}
	if !bound {
		return "", ACPRouterError{Code: "session_route_conflict", Message: "session/new route bind conflicted"}
	}
	log.Printf(
		"[paxd] acp route bound connection_id=%s native_session_id=%s slot_id=%s process_epoch=%s operation=session_new",
		r.connectionID,
		sessionID,
		slotID,
		processEpoch,
	)
	_ = payload
	return sessionID, nil
}

func (r *ACPRouter) resumeColdRoute(ctx context.Context, slot ACPRouterSlot, route ACPRoute) error {
	resumeDescriptor, err := r.localizeSessionLifecycleParams(route.ResumeParams)
	if err != nil {
		return err
	}
	resumeParams, err := resumeParamsForSession(route.NativeSessionID, resumeDescriptor)
	if err != nil {
		return err
	}
	log.Printf(
		"[paxd] acp route resume starting connection_id=%s native_session_id=%s last_slot_id=%s target_slot_id=%s target_process_epoch=%s",
		r.connectionID,
		route.NativeSessionID,
		route.LastSlotID,
		slot.SlotID(),
		slot.ProcessEpoch(),
	)
	id := r.nextResumeID()
	request, err := json.Marshal(acpRPCMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%q", id)),
		Method:  "session/resume",
		Params:  resumeParams,
	})
	if err != nil {
		return err
	}
	waiter := r.registerInternalWaiter(slot.SlotID(), slot.ProcessEpoch(), json.RawMessage(fmt.Sprintf("%q", id)))
	if err := slot.Send(ctx, request); err != nil {
		r.dropInternalWaiter(slot.SlotID(), slot.ProcessEpoch(), json.RawMessage(fmt.Sprintf("%q", id)))
		return err
	}
	select {
	case result := <-waiter:
		if result.err != nil {
			return result.err
		}
		if result.msg.Error != nil {
			return ACPRouterError{Code: "session_resume_failed", Message: result.msg.Error.Message}
		}
	case <-ctx.Done():
		r.dropInternalWaiter(slot.SlotID(), slot.ProcessEpoch(), json.RawMessage(fmt.Sprintf("%q", id)))
		return ctx.Err()
	}
	_, bound, err := r.store.BindACPSessionRoute(ctx, ACPRouteBindingUpdate{
		ConnectionID:    route.ConnectionID,
		NativeSessionID: route.NativeSessionID,
		SlotID:          slot.SlotID(),
		ProcessEpoch:    slot.ProcessEpoch(),
		ExpectedVersion: route.Version,
		ResumeParams:    resumeDescriptor,
	})
	if err != nil {
		return err
	}
	if !bound {
		return ACPRouterError{Code: "session_route_conflict", Message: "resume route bind conflicted"}
	}
	log.Printf(
		"[paxd] acp route resume complete connection_id=%s native_session_id=%s slot_id=%s process_epoch=%s",
		r.connectionID,
		route.NativeSessionID,
		slot.SlotID(),
		slot.ProcessEpoch(),
	)
	return nil
}

func (r *ACPRouter) localizeSessionLifecycleMessage(msg acpRPCMessage) (acpRPCMessage, []byte, error) {
	params, err := r.localizeSessionLifecycleParams(msg.Params)
	if err != nil {
		return acpRPCMessage{}, nil, err
	}
	msg.Params = params
	payload, err := json.Marshal(msg)
	if err != nil {
		return acpRPCMessage{}, nil, err
	}
	return msg, payload, nil
}

func (r *ACPRouter) localizeSessionLifecycleParams(params json.RawMessage) (json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil {
		return nil, ACPRouterError{Code: "invalid_session_lifecycle", Message: "session lifecycle params must be an object"}
	}
	cwd := stringField(raw, "cwd")
	if cwd != "" {
		resolvedCWD, err := r.workspace(cwd)
		if err != nil {
			return nil, err
		}
		raw["cwd"], err = json.Marshal(resolvedCWD)
		if err != nil {
			return nil, ACPRouterError{
				Code:    "workspace_path_invalid",
				Message: "resolved workspace path cannot be encoded",
			}
		}
	}
	mcpServers, ok := firstJSONField(raw, "mcpServers", "mcp_servers")
	if !ok {
		raw["mcpServers"] = json.RawMessage(`[]`)
		delete(raw, "mcp_servers")
		return canonicalJSON(raw)
	}
	localizedServers, err := r.localizeMCPServers(mcpServers)
	if err != nil {
		return nil, err
	}
	raw["mcpServers"] = localizedServers
	delete(raw, "mcp_servers")
	return canonicalJSON(raw)
}

func (r *ACPRouter) localizeMCPServers(mcpServers json.RawMessage) (json.RawMessage, error) {
	var servers []map[string]json.RawMessage
	if err := json.Unmarshal(mcpServers, &servers); err != nil {
		return append(json.RawMessage(nil), mcpServers...), nil
	}
	var executable string
	for i := range servers {
		if !isPaxConversationMCPName(stringField(servers[i], "name")) {
			continue
		}
		if executable == "" {
			path, err := r.executable()
			if err != nil {
				return nil, fmt.Errorf("resolve paxd executable path: %w", err)
			}
			executable = strings.TrimSpace(path)
			if executable == "" {
				return nil, ACPRouterError{Code: "paxd_executable_missing", Message: "paxd executable path is empty"}
			}
		}
		command, _ := json.Marshal(executable)
		args, _ := json.Marshal([]string{"mcp", "conversation", "serve"})
		servers[i]["command"] = command
		servers[i]["args"] = args
	}
	payload, err := json.Marshal(servers)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(payload), nil
}

func isPaxConversationMCPName(name string) bool {
	if name == paxConversationMCPName {
		return true
	}
	suffix, ok := strings.CutPrefix(name, paxConversationMCPName+"-")
	if !ok || len(suffix) != paxConversationMCPNameSuffixLength {
		return false
	}
	for _, char := range suffix {
		if (char < 'a' || char > 'z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}

func (r *ACPRouter) selectReadySlot(ctx context.Context) (ACPRouterSlot, error) {
	boundCounts, err := r.store.CountBoundACPSessionRoutesBySlot(ctx, r.connectionID)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slots := make([]ACPRouterSlot, 0, len(r.slots))
	for _, slot := range r.slots {
		if slot.Ready() && !r.slotDrainingLocked(slot) {
			slots = append(slots, slot)
		}
	}
	if len(slots) == 0 {
		return nil, ACPRouterError{Code: "slot_unavailable", Message: "no ready ACP slot"}
	}
	sort.Slice(slots, func(i, j int) bool {
		busyI := r.activeSlotPrompts[slots[i].SlotID()] != ""
		busyJ := r.activeSlotPrompts[slots[j].SlotID()] != ""
		if busyI != busyJ {
			return !busyI
		}
		promptI := r.lastPrompt[slots[i].SlotID()]
		promptJ := r.lastPrompt[slots[j].SlotID()]
		if promptI != promptJ {
			return promptI < promptJ
		}
		countI := boundCounts[slots[i].SlotID()] + r.slotReservations[slots[i].SlotID()]
		countJ := boundCounts[slots[j].SlotID()] + r.slotReservations[slots[j].SlotID()]
		if countI != countJ {
			return countI < countJ
		}
		assignedI := r.lastAssigned[slots[i].SlotID()]
		assignedJ := r.lastAssigned[slots[j].SlotID()]
		if assignedI != assignedJ {
			return assignedI < assignedJ
		}
		return slots[i].Ordinal() < slots[j].Ordinal()
	})
	selected := slots[0]
	r.slotReservations[selected.SlotID()]++
	r.assignmentSeq++
	r.lastAssigned[selected.SlotID()] = r.assignmentSeq
	return selected, nil
}

func (r *ACPRouter) recordPromptAccepted(slotID string) {
	r.mu.Lock()
	r.promptSeq++
	r.lastPrompt[slotID] = r.promptSeq
	r.mu.Unlock()
}

func (r *ACPRouter) releaseSlotReservation(slotID string) {
	r.mu.Lock()
	if r.slotReservations[slotID] > 0 {
		r.slotReservations[slotID]--
	}
	r.signalSlotDrainIfIdleLocked(slotID)
	r.mu.Unlock()
}

func (r *ACPRouter) hotSlot(route ACPRoute, allowDraining bool) (ACPRouterSlot, bool) {
	if route.BoundSlotID == "" || route.BoundProcessEpoch == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[route.BoundSlotID]
	if slot == nil || !slot.Ready() || slot.ProcessEpoch() != route.BoundProcessEpoch {
		return nil, false
	}
	if !allowDraining && r.slotDrainingLocked(slot) {
		return nil, false
	}
	return slot, true
}

func (r *ACPRouter) routeSlotDraining(route ACPRoute) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	drain := r.drainingSlots[route.BoundSlotID]
	return drain != nil && drain.processEpoch == route.BoundProcessEpoch
}

func (r *ACPRouter) acquirePromptLease(nativeSessionID string, slot ACPRouterSlot, requestID json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeSessionTurns[nativeSessionID] != "" {
		return ACPRouterError{Code: "session_busy", Message: "native session already has an active prompt"}
	}
	if r.activeSlotPrompts[slot.SlotID()] != "" {
		return ACPRouterError{Code: "slot_busy", Message: "ACP slot already has an active prompt"}
	}
	key := rpcIDKey(requestID)
	if key == "" {
		return ACPRouterError{Code: "request_id_required", Message: "session/prompt requires an id"}
	}
	r.activeSessionTurns[nativeSessionID] = key
	r.activeSlotPrompts[slot.SlotID()] = nativeSessionID
	r.pendingPrompts[key] = pendingPrompt{
		nativeSessionID: nativeSessionID,
		slotID:          slot.SlotID(),
		processEpoch:    slot.ProcessEpoch(),
	}
	return nil
}

func (r *ACPRouter) releasePromptForResponse(requestID json.RawMessage, slotID string, processEpoch string) string {
	key := rpcIDKey(requestID)
	r.mu.Lock()
	pending, ok := r.pendingPrompts[key]
	if ok && pending.slotID == slotID && pending.processEpoch == processEpoch {
		delete(r.pendingPrompts, key)
		delete(r.activeSessionTurns, pending.nativeSessionID)
		delete(r.activeSlotPrompts, slotID)
		r.signalSlotDrainIfIdleLocked(slotID)
	}
	r.mu.Unlock()
	if ok && pending.slotID == slotID && pending.processEpoch == processEpoch {
		return pending.nativeSessionID
	}
	return ""
}

func (r *ACPRouter) releasePrompt(nativeSessionID string, slotID string, requestKey string) {
	r.mu.Lock()
	delete(r.pendingPrompts, requestKey)
	delete(r.activeSessionTurns, nativeSessionID)
	delete(r.activeSlotPrompts, slotID)
	r.signalSlotDrainIfIdleLocked(slotID)
	r.mu.Unlock()
}

func (r *ACPRouter) slotDrainingLocked(slot ACPRouterSlot) bool {
	drain := r.drainingSlots[slot.SlotID()]
	return drain != nil && drain.processEpoch == slot.ProcessEpoch()
}

func (r *ACPRouter) signalSlotDrainIfIdleLocked(slotID string) {
	drain := r.drainingSlots[slotID]
	if drain == nil || drain.closed || r.slotReservations[slotID] > 0 || r.activeSlotPrompts[slotID] != "" {
		return
	}
	for _, pending := range r.pendingNew {
		if pending.slotID == slotID && pending.processEpoch == drain.processEpoch {
			return
		}
	}
	for _, pending := range r.pendingWorkerReqs {
		if pending.slotID == slotID && pending.processEpoch == drain.processEpoch {
			return
		}
	}
	waiterPrefix := slotID + "|" + drain.processEpoch + "|"
	for key := range r.waiters {
		if strings.HasPrefix(key, waiterPrefix) {
			return
		}
	}
	r.closeSlotDrainLocked(drain)
}

func (r *ACPRouter) closeSlotDrainLocked(drain *slotDrain) {
	if drain == nil || drain.closed {
		return
	}
	drain.closed = true
	close(drain.done)
}

func (r *ACPRouter) notifySlotDrain(slotID string) {
	r.mu.Lock()
	r.signalSlotDrainIfIdleLocked(slotID)
	r.mu.Unlock()
}

func (r *ACPRouter) nextResumeID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumeSeq++
	return fmt.Sprintf("%s%d", internalACPResumeIDPrefix, r.resumeSeq)
}

func (r *ACPRouter) registerInternalWaiter(slotID string, processEpoch string, id json.RawMessage) chan acpRPCWaitResult {
	key := internalWaiterKey(slotID, processEpoch, id)
	ch := make(chan acpRPCWaitResult, 1)
	r.mu.Lock()
	r.waiters[key] = ch
	r.mu.Unlock()
	return ch
}

func (r *ACPRouter) dropInternalWaiter(slotID string, processEpoch string, id json.RawMessage) {
	key := internalWaiterKey(slotID, processEpoch, id)
	r.mu.Lock()
	delete(r.waiters, key)
	r.mu.Unlock()
}

func (r *ACPRouter) resolveInternalWaiter(slotID string, processEpoch string, msg acpRPCMessage) bool {
	if msg.Method != "" || len(bytes.TrimSpace(msg.ID)) == 0 {
		return false
	}
	key := internalWaiterKey(slotID, processEpoch, msg.ID)
	r.mu.Lock()
	ch := r.waiters[key]
	if ch != nil {
		delete(r.waiters, key)
	}
	r.mu.Unlock()
	if ch == nil {
		return false
	}
	ch <- acpRPCWaitResult{msg: msg}
	return true
}

func internalWaiterKey(slotID string, processEpoch string, id json.RawMessage) string {
	return slotID + "|" + processEpoch + "|" + rpcIDKey(id)
}

func workerRequestKey(nativeSessionID string, id json.RawMessage) string {
	return nativeSessionID + "\x00" + rpcIDKey(id)
}

func firstSessionID(fallback string, params json.RawMessage) string {
	if strings.TrimSpace(fallback) != "" {
		return strings.TrimSpace(fallback)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(params, &raw); err != nil {
		return ""
	}
	for _, key := range []string{"sessionId", "session_id"} {
		var value string
		if err := json.Unmarshal(raw[key], &value); err == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sessionIDFromResult(result json.RawMessage) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(result, &raw); err != nil {
		return ""
	}
	for _, key := range []string{"sessionId", "session_id"} {
		var value string
		if err := json.Unmarshal(raw[key], &value); err == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func resumeDescriptorFromSessionNewParams(requestParams json.RawMessage) (json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(requestParams, &raw); err != nil {
		return nil, ACPRouterError{Code: "invalid_session_lifecycle", Message: "session/new params must be an object"}
	}
	cwd := stringField(raw, "cwd")
	if cwd == "" {
		return nil, ACPRouterError{Code: "invalid_session_lifecycle", Message: "session/new params require cwd"}
	}
	cwdJSON, _ := json.Marshal(cwd)
	descriptor := map[string]json.RawMessage{"cwd": cwdJSON}
	mcpServers, ok := firstJSONField(raw, "mcpServers", "mcp_servers")
	if !ok {
		mcpServers = json.RawMessage(`[]`)
	}
	descriptor["mcpServers"] = mcpServers
	additionalDirectories, ok := firstJSONField(raw, "additionalDirectories", "additional_directories")
	if ok {
		descriptor["additionalDirectories"] = additionalDirectories
	}
	payload, err := canonicalJSON(descriptor)
	if err != nil {
		return nil, ACPRouterError{Code: "invalid_session_lifecycle", Message: "session/new lifecycle descriptor cannot be encoded"}
	}
	return payload, nil
}

func resumeParamsForSession(sessionID string, descriptor json.RawMessage) (json.RawMessage, error) {
	if strings.TrimSpace(sessionID) == "" || len(bytes.TrimSpace(descriptor)) == 0 {
		return nil, ACPRouterError{Code: "session_route_missing", Message: "route has no complete resume descriptor"}
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(descriptor, &params); err != nil || stringField(params, "cwd") == "" {
		return nil, ACPRouterError{Code: "session_route_missing", Message: "route has no complete resume descriptor"}
	}
	if _, ok := params["mcpServers"]; !ok {
		return nil, ACPRouterError{Code: "session_route_missing", Message: "route has no complete resume descriptor"}
	}
	sessionIDJSON, err := json.Marshal(sessionID)
	if err != nil {
		return nil, err
	}
	delete(params, "session_id")
	params["sessionId"] = sessionIDJSON
	payload, err := canonicalJSON(params)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func firstJSONField(raw map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		value := raw[key]
		if len(bytes.TrimSpace(value)) == 0 {
			continue
		}
		return append(json.RawMessage(nil), value...), true
	}
	return nil, false
}

func stringField(raw map[string]json.RawMessage, key string) string {
	var value string
	if err := json.Unmarshal(raw[key], &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}
