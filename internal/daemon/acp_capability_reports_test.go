package daemon

import (
	"context"
	"testing"
	"time"

	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/stretchr/testify/require"
)

func TestACPCapabilityReportsStoresRuntimeReportForControlSnapshot(t *testing.T) {
	ctx := context.Background()
	reports := newACPCapabilityReports()
	initializedAt := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)

	err := reports.ReportACPPoolCapability(ctx, runtimes.ACPPoolCapabilityReport{
		SchemaVersion:        1,
		ConnectionID:         "conn_codex",
		ReportGeneration:     7,
		PaxdVersion:          "dev",
		CommandFingerprint:   "fingerprint_1",
		ClientProfileHash:    "profile_hash_1",
		WorkerResultHash:     "worker_hash_1",
		ProtocolVersion:      1,
		ClientCapabilityKeys: []string{"fs"},
		WorkerCapabilityKeys: []string{"prompt"},
		InitPhase:            "ready",
		InitializedAt:        initializedAt,
	})

	require.NoError(t, err)
	report, ok := reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.True(t, ok)
	require.Equal(t, "dev", report.PaxdVersion)
	require.Equal(t, "worker_hash_1", report.WorkerResultHash)
	require.Equal(t, []string{"prompt"}, report.WorkerCapabilityKeys)
	require.Equal(t, initializedAt, report.InitializedAt)

	report.WorkerCapabilityKeys[0] = "mutated"
	report, ok = reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.True(t, ok)
	require.Equal(t, []string{"prompt"}, report.WorkerCapabilityKeys)
}
