package daemon

import (
	"context"
	"sync"

	"github.com/pax-beehive/paxd/internal/control"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
)

type acpCapabilityReports struct {
	mu          sync.RWMutex
	reports     map[string]control.ACPPoolCapabilityReport
	slots       map[string]map[string]acpSlotCapability
	generations map[string]int64
}

type acpSlotCapability struct {
	processEpoch string
	ordinal      int
	report       runtimes.ACPPoolCapabilityReport
}

func newACPCapabilityReports() *acpCapabilityReports {
	return &acpCapabilityReports{
		reports:     make(map[string]control.ACPPoolCapabilityReport),
		slots:       make(map[string]map[string]acpSlotCapability),
		generations: make(map[string]int64),
	}
}

func (r *acpCapabilityReports) ReportACPPoolCapability(ctx context.Context, report runtimes.ACPPoolCapabilityReport) error {
	_ = ctx
	if r == nil || report.ConnectionID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports[report.ConnectionID] = controlCapabilityReport(report.WithDefaults())
	return nil
}

func (r *acpCapabilityReports) ReportACPSlotCapability(
	ctx context.Context,
	spec runtimes.ACPSlotSpec,
	report *runtimes.ACPPoolCapabilityReport,
) error {
	_ = ctx
	if r == nil || spec.ConnectionID == "" || spec.SlotID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.slots == nil {
		r.slots = make(map[string]map[string]acpSlotCapability)
	}
	if r.generations == nil {
		r.generations = make(map[string]int64)
	}
	slots := r.slots[spec.ConnectionID]
	changed := false
	if report == nil {
		if current, ok := slots[spec.SlotID]; ok &&
			(spec.ProcessEpoch == "" || current.processEpoch == spec.ProcessEpoch) {
			delete(slots, spec.SlotID)
			changed = true
		}
	} else {
		if slots == nil {
			slots = make(map[string]acpSlotCapability)
			r.slots[spec.ConnectionID] = slots
		}
		copy := cloneRuntimeCapabilityReport(report.WithDefaults())
		slots[spec.SlotID] = acpSlotCapability{
			processEpoch: spec.ProcessEpoch,
			ordinal:      spec.Ordinal,
			report:       copy,
		}
		changed = true
	}
	if !changed {
		return nil
	}
	r.rebuildPoolReportLocked(spec.ConnectionID)
	return nil
}

func (r *acpCapabilityReports) rebuildPoolReportLocked(connectionID string) {
	slots := r.slots[connectionID]
	if len(slots) == 0 {
		delete(r.slots, connectionID)
		delete(r.reports, connectionID)
		return
	}
	var selected acpSlotCapability
	selectedSet := false
	for _, candidate := range slots {
		if candidate.report.InitPhase != runtimes.ACPPoolInitPhaseReady {
			continue
		}
		if !selectedSet || candidate.ordinal < selected.ordinal ||
			(candidate.ordinal == selected.ordinal && candidate.processEpoch < selected.processEpoch) {
			selected = candidate
			selectedSet = true
		}
	}
	if !selectedSet {
		delete(r.reports, connectionID)
		return
	}
	r.generations[connectionID]++
	report := selected.report
	report.ConnectionID = connectionID
	report.ReportGeneration = r.generations[connectionID]
	report.InitPhase = runtimes.ACPPoolInitPhaseReady
	report.PoolConsistency = poolIdentityConsistency(slots)
	r.reports[connectionID] = controlCapabilityReport(report)
}

func poolIdentityConsistency(slots map[string]acpSlotCapability) string {
	fingerprints := make(map[string]struct{})
	readyCount := 0
	missingIdentity := false
	for _, slot := range slots {
		if slot.report.InitPhase != runtimes.ACPPoolInitPhaseReady {
			continue
		}
		readyCount++
		fingerprint := ""
		if slot.report.Implementation != nil {
			fingerprint = slot.report.Implementation.IdentityFingerprint
		}
		if fingerprint == "" {
			missingIdentity = true
			continue
		}
		fingerprints[fingerprint] = struct{}{}
	}
	if len(fingerprints) > 1 {
		return runtimes.ACPPoolConsistencyMixed
	}
	if readyCount == 0 || missingIdentity || len(fingerprints) == 0 {
		return runtimes.ACPPoolConsistencyUnknown
	}
	return runtimes.ACPPoolConsistencyConsistent
}

func controlCapabilityReport(report runtimes.ACPPoolCapabilityReport) control.ACPPoolCapabilityReport {
	return control.ACPPoolCapabilityReport{
		SchemaVersion:        report.SchemaVersion,
		ConnectionID:         report.ConnectionID,
		ReportGeneration:     report.ReportGeneration,
		PaxdVersion:          report.PaxdVersion,
		CommandFingerprint:   report.CommandFingerprint,
		ClientProfileHash:    report.ClientProfileHash,
		WorkerResultHash:     report.WorkerResultHash,
		ProtocolVersion:      report.ProtocolVersion,
		ClientCapabilityKeys: append([]string(nil), report.ClientCapabilityKeys...),
		WorkerCapabilityKeys: append([]string(nil), report.WorkerCapabilityKeys...),
		Implementation:       controlImplementationIdentity(report.Implementation),
		PoolConsistency:      report.PoolConsistency,
		InitPhase:            report.InitPhase,
		InitializedAt:        report.InitializedAt,
		LastErrorCode:        report.LastErrorCode,
		LastErrorMessage:     report.LastErrorMessage,
	}
}

func (r *acpCapabilityReports) ACPPoolCapabilityReport(ctx context.Context, connectionID string) (*control.ACPPoolCapabilityReport, bool) {
	_ = ctx
	if r == nil || connectionID == "" {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	report, ok := r.reports[connectionID]
	if !ok {
		return nil, false
	}
	report.ClientCapabilityKeys = append([]string(nil), report.ClientCapabilityKeys...)
	report.WorkerCapabilityKeys = append([]string(nil), report.WorkerCapabilityKeys...)
	report.Implementation = cloneControlImplementationIdentity(report.Implementation)
	return &report, true
}

func cloneRuntimeCapabilityReport(report runtimes.ACPPoolCapabilityReport) runtimes.ACPPoolCapabilityReport {
	report.ClientCapabilityKeys = append([]string(nil), report.ClientCapabilityKeys...)
	report.WorkerCapabilityKeys = append([]string(nil), report.WorkerCapabilityKeys...)
	if report.Implementation != nil {
		identity := *report.Implementation
		if identity.ACPAgent != nil {
			agent := *identity.ACPAgent
			identity.ACPAgent = &agent
		}
		if identity.Runtime != nil {
			runtime := *identity.Runtime
			identity.Runtime = &runtime
		}
		report.Implementation = &identity
	}
	return report
}

func controlImplementationIdentity(identity *runtimes.ACPImplementationIdentity) *control.ACPImplementationIdentity {
	if identity == nil {
		return nil
	}
	out := &control.ACPImplementationIdentity{
		IdentityFingerprint: identity.IdentityFingerprint,
	}
	if identity.ACPAgent != nil {
		out.ACPAgent = &control.ACPAgentImplementation{
			Name:    identity.ACPAgent.Name,
			Title:   identity.ACPAgent.Title,
			Version: identity.ACPAgent.Version,
		}
	}
	if identity.Runtime != nil {
		out.Runtime = &control.ACPRuntimeImplementation{
			Name:    identity.Runtime.Name,
			Version: identity.Runtime.Version,
			Build:   identity.Runtime.Build,
			Channel: identity.Runtime.Channel,
		}
	}
	return out
}

func cloneControlImplementationIdentity(identity *control.ACPImplementationIdentity) *control.ACPImplementationIdentity {
	if identity == nil {
		return nil
	}
	out := *identity
	if identity.ACPAgent != nil {
		agent := *identity.ACPAgent
		out.ACPAgent = &agent
	}
	if identity.Runtime != nil {
		runtime := *identity.Runtime
		out.Runtime = &runtime
	}
	return &out
}
