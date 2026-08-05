package runtime

import (
	"sync"
	"time"
)

type DaemonDrainLease struct {
	CommandID string
	BootID    string
	ExpiresAt time.Time
}

type DaemonActivitySnapshot struct {
	ActiveTurns       int `json:"active_turns"`
	PendingLifecycle  int `json:"pending_lifecycle"`
	PendingWorkerRPCs int `json:"pending_worker_rpcs"`
	SlotReservations  int `json:"slot_reservations"`
	InFlightAdmission int `json:"in_flight_admission"`
}

func (s DaemonActivitySnapshot) Idle() bool {
	return s.ActiveTurns == 0 && s.PendingLifecycle == 0 && s.PendingWorkerRPCs == 0 &&
		s.SlotReservations == 0 && s.InFlightAdmission == 0
}

func (r *ACPPoolRegistry) SetBootID(bootID string) {
	if r == nil {
		return
	}
	r.admissionMu.Lock()
	r.bootID = bootID
	r.admissionMu.Unlock()
}

func (r *ACPPoolRegistry) BeginDrain(lease DaemonDrainLease) {
	if r == nil {
		return
	}
	r.admissionMu.Lock()
	if lease.BootID == r.bootID {
		copy := lease
		r.drain = &copy
	}
	r.admissionMu.Unlock()
	r.notifyActivity()
}

func (r *ACPPoolRegistry) ClearDrain(commandID string) {
	if r == nil {
		return
	}
	r.admissionMu.Lock()
	if r.drain != nil && r.drain.CommandID == commandID {
		r.drain = nil
	}
	if r.cutover == commandID {
		r.cutover = ""
	}
	r.admissionMu.Unlock()
	r.notifyActivity()
}

func (r *ACPPoolRegistry) CloseForCutover(commandID string, force bool) bool {
	if r == nil {
		return true
	}
	r.admissionMu.Lock()
	defer r.admissionMu.Unlock()
	if r.cutover != "" && r.cutover != commandID {
		return false
	}
	if !force && !r.activitySnapshotLocked().Idle() {
		return false
	}
	r.cutover = commandID
	return true
}

func (r *ACPPoolRegistry) ActivitySnapshot() DaemonActivitySnapshot {
	if r == nil {
		return DaemonActivitySnapshot{}
	}
	r.admissionMu.Lock()
	defer r.admissionMu.Unlock()
	return r.activitySnapshotLocked()
}

func (r *ACPPoolRegistry) SubscribeActivity() (<-chan struct{}, func()) {
	return r.SubscribeSessionRuntime("")
}

func (r *ACPPoolRegistry) activitySnapshotLocked() DaemonActivitySnapshot {
	snapshot := DaemonActivitySnapshot{InFlightAdmission: r.inFlight}
	r.mu.Lock()
	pools := make([]*ACPPool, 0, len(r.pools))
	for _, pool := range r.pools {
		pools = append(pools, pool)
	}
	r.mu.Unlock()
	for _, pool := range pools {
		part := pool.router.ActivitySnapshot()
		snapshot.ActiveTurns += part.ActiveTurns
		snapshot.PendingLifecycle += part.PendingLifecycle
		snapshot.PendingWorkerRPCs += part.PendingWorkerRPCs
		snapshot.SlotReservations += part.SlotReservations
	}
	return snapshot
}

func (r *ACPPoolRegistry) beginAdmission(payload []byte) (func(), error) {
	if r == nil {
		return func() {}, nil
	}
	msg, ok := parseACPRPCMessage(payload)
	if !ok || !maintenanceAdmissionMethod(msg.Method) {
		return func() {}, nil
	}
	r.admissionMu.Lock()
	now := time.Now()
	if r.drain != nil && (!r.drain.ExpiresAt.After(now) || r.drain.BootID != r.bootID) {
		r.drain = nil
	}
	if r.cutover != "" {
		r.admissionMu.Unlock()
		return nil, ACPRouterError{Code: "daemon_restarting", Message: "daemon restart is committed"}
	}
	if r.drain != nil {
		r.admissionMu.Unlock()
		return nil, ACPRouterError{Code: "daemon_draining", Message: "daemon is draining for maintenance"}
	}
	r.inFlight++
	r.admissionMu.Unlock()
	r.notifyActivity()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.admissionMu.Lock()
			if r.inFlight > 0 {
				r.inFlight--
			}
			r.admissionMu.Unlock()
			r.notifyActivity()
		})
	}, nil
}

func maintenanceAdmissionMethod(method string) bool {
	switch method {
	case "session/new", "session/resume", "session/load", "session/prompt":
		return true
	default:
		return false
	}
}

func (r *ACPPoolRegistry) notifyActivity() {
	if r == nil {
		return
	}
	r.mu.Lock()
	for _, subscriber := range r.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
	r.mu.Unlock()
}

func (r *ACPRouter) ActivitySnapshot() DaemonActivitySnapshot {
	if r == nil {
		return DaemonActivitySnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	reservations := 0
	for _, count := range r.slotReservations {
		reservations += count
	}
	return DaemonActivitySnapshot{
		ActiveTurns: len(r.activeSessionTurns), PendingLifecycle: len(r.pendingNew),
		PendingWorkerRPCs: len(r.pendingWorkerReqs), SlotReservations: reservations,
	}
}
