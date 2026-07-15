package daemon

import (
	"context"
	"sync"

	"github.com/pax-beehive/paxd/internal/control"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
)

type acpCapabilityReports struct {
	mu      sync.RWMutex
	reports map[string]control.ACPPoolCapabilityReport
}

func newACPCapabilityReports() *acpCapabilityReports {
	return &acpCapabilityReports{reports: make(map[string]control.ACPPoolCapabilityReport)}
}

func (r *acpCapabilityReports) ReportACPPoolCapability(ctx context.Context, report runtimes.ACPPoolCapabilityReport) error {
	_ = ctx
	if r == nil || report.ConnectionID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports[report.ConnectionID] = control.ACPPoolCapabilityReport{
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
		InitPhase:            report.InitPhase,
		InitializedAt:        report.InitializedAt,
		LastErrorCode:        report.LastErrorCode,
		LastErrorMessage:     report.LastErrorMessage,
	}
	return nil
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
	return &report, true
}
