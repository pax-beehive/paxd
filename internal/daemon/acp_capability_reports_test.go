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
		SchemaVersion:        runtimes.ACPPoolCapabilityReportSchemaVersion,
		ConnectionID:         "conn_codex",
		ReportGeneration:     7,
		PaxdVersion:          "dev",
		CommandFingerprint:   "fingerprint_1",
		ClientProfileHash:    "profile_hash_1",
		WorkerResultHash:     "worker_hash_1",
		ProtocolVersion:      1,
		ClientCapabilityKeys: []string{"fs"},
		WorkerCapabilityKeys: []string{"prompt"},
		Implementation: &runtimes.ACPImplementationIdentity{
			ACPAgent:            &runtimes.ACPAgentImplementation{Name: "hermes-agent", Version: "0.17.0"},
			IdentityFingerprint: "identity_1",
		},
		InitPhase:     "ready",
		InitializedAt: initializedAt,
	})

	require.NoError(t, err)
	report, ok := reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.True(t, ok)
	require.Equal(t, "dev", report.PaxdVersion)
	require.Equal(t, "worker_hash_1", report.WorkerResultHash)
	require.Equal(t, []string{"prompt"}, report.WorkerCapabilityKeys)
	require.Equal(t, initializedAt, report.InitializedAt)
	require.Equal(t, runtimes.ACPPoolConsistencyConsistent, report.PoolConsistency)
	require.NotNil(t, report.Implementation)
	require.NotNil(t, report.Implementation.ACPAgent)
	require.Equal(t, "hermes-agent", report.Implementation.ACPAgent.Name)

	report.WorkerCapabilityKeys[0] = "mutated"
	report.Implementation.ACPAgent.Name = "mutated"
	report, ok = reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.True(t, ok)
	require.Equal(t, []string{"prompt"}, report.WorkerCapabilityKeys)
	require.Equal(t, "hermes-agent", report.Implementation.ACPAgent.Name)
}

func TestACPCapabilityReportsAggregatesReadySlotsAndIgnoresStaleRemoval(t *testing.T) {
	ctx := context.Background()
	reports := newACPCapabilityReports()
	firstInitializedAt := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	secondInitializedAt := firstInitializedAt.Add(time.Second)

	firstSpec := runtimes.ACPSlotSpec{
		ConnectionID: "conn_codex",
		SlotID:       "conn_codex:0",
		Ordinal:      0,
		ProcessEpoch: "epoch_1",
	}
	secondSpec := runtimes.ACPSlotSpec{
		ConnectionID: "conn_codex",
		SlotID:       "conn_codex:1",
		Ordinal:      1,
		ProcessEpoch: "epoch_2",
	}
	require.NoError(t, reports.ReportACPSlotCapability(ctx, firstSpec, &runtimes.ACPPoolCapabilityReport{
		ConnectionID:       "conn_codex",
		WorkerResultHash:   "worker_1",
		InitPhase:          runtimes.ACPPoolInitPhaseReady,
		InitializedAt:      firstInitializedAt,
		CommandFingerprint: "fingerprint_1",
		Implementation: &runtimes.ACPImplementationIdentity{
			ACPAgent:            &runtimes.ACPAgentImplementation{Name: "codex-acp", Version: "1.1.7"},
			IdentityFingerprint: "identity_1",
		},
	}))
	require.NoError(t, reports.ReportACPSlotCapability(ctx, secondSpec, &runtimes.ACPPoolCapabilityReport{
		ConnectionID:       "conn_codex",
		WorkerResultHash:   "worker_2",
		InitPhase:          runtimes.ACPPoolInitPhaseReady,
		InitializedAt:      secondInitializedAt,
		CommandFingerprint: "fingerprint_1",
		Implementation: &runtimes.ACPImplementationIdentity{
			ACPAgent:            &runtimes.ACPAgentImplementation{Name: "codex-acp", Version: "1.1.8"},
			IdentityFingerprint: "identity_2",
		},
	}))

	report, ok := reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.True(t, ok)
	require.Equal(t, int64(2), report.ReportGeneration)
	require.Equal(t, "worker_1", report.WorkerResultHash)
	require.Equal(t, firstInitializedAt, report.InitializedAt)
	require.Equal(t, runtimes.ACPPoolConsistencyMixed, report.PoolConsistency)
	require.NotNil(t, report.Implementation)
	require.Equal(t, "identity_1", report.Implementation.IdentityFingerprint)

	require.NoError(t, reports.ReportACPSlotCapability(ctx, firstSpec, nil))
	report, ok = reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.True(t, ok)
	require.Equal(t, int64(3), report.ReportGeneration)
	require.Equal(t, "worker_2", report.WorkerResultHash)
	require.Equal(t, runtimes.ACPPoolConsistencyConsistent, report.PoolConsistency)

	replacementSpec := secondSpec
	replacementSpec.ProcessEpoch = "epoch_3"
	require.NoError(t, reports.ReportACPSlotCapability(ctx, replacementSpec, &runtimes.ACPPoolCapabilityReport{
		ConnectionID:       "conn_codex",
		WorkerResultHash:   "worker_3",
		InitPhase:          runtimes.ACPPoolInitPhaseReady,
		InitializedAt:      secondInitializedAt.Add(time.Second),
		CommandFingerprint: "fingerprint_1",
		Implementation: &runtimes.ACPImplementationIdentity{
			ACPAgent:            &runtimes.ACPAgentImplementation{Name: "codex-acp", Version: "1.1.9"},
			IdentityFingerprint: "identity_3",
		},
	}))
	require.NoError(t, reports.ReportACPSlotCapability(ctx, secondSpec, nil))
	report, ok = reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.True(t, ok)
	require.Equal(t, int64(4), report.ReportGeneration)
	require.Equal(t, "worker_3", report.WorkerResultHash)
	require.Equal(t, runtimes.ACPPoolConsistencyConsistent, report.PoolConsistency)

	require.NoError(t, reports.ReportACPSlotCapability(ctx, replacementSpec, nil))
	_, ok = reports.ACPPoolCapabilityReport(ctx, "conn_codex")
	require.False(t, ok)
}

func TestACPPoolIdentityConsistency(t *testing.T) {
	t.Run("Given ready slots with different implementation fingerprints when the pool report is rebuilt then it is marked mixed", func(t *testing.T) {
		reports := newACPCapabilityReports()
		ctx := context.Background()
		first := runtimes.ACPSlotSpec{ConnectionID: "conn_codex", SlotID: "slot_0", Ordinal: 0, ProcessEpoch: "epoch_0"}
		second := runtimes.ACPSlotSpec{ConnectionID: "conn_codex", SlotID: "slot_1", Ordinal: 1, ProcessEpoch: "epoch_1"}

		require.NoError(t, reports.ReportACPSlotCapability(ctx, first, &runtimes.ACPPoolCapabilityReport{
			InitPhase: runtimes.ACPPoolInitPhaseReady,
			Implementation: &runtimes.ACPImplementationIdentity{
				ACPAgent:            &runtimes.ACPAgentImplementation{Name: "codex-acp", Version: "1.1.7"},
				IdentityFingerprint: "identity_1",
			},
		}))
		require.NoError(t, reports.ReportACPSlotCapability(ctx, second, &runtimes.ACPPoolCapabilityReport{
			InitPhase: runtimes.ACPPoolInitPhaseReady,
			Implementation: &runtimes.ACPImplementationIdentity{
				ACPAgent:            &runtimes.ACPAgentImplementation{Name: "codex-acp", Version: "1.1.8"},
				IdentityFingerprint: "identity_2",
			},
		}))

		report, ok := reports.ACPPoolCapabilityReport(ctx, "conn_codex")
		require.True(t, ok)
		require.Equal(t, runtimes.ACPPoolConsistencyMixed, report.PoolConsistency)
	})
}

func TestACPSlotCapabilityWriterPokesOwningRemote(t *testing.T) {
	reports := newACPCapabilityReports()
	status := newStatusHub()
	pokes, unsubscribe := status.Subscribe("local")
	defer unsubscribe()
	writer := acpSlotCapabilityWriter(reports, status)

	writer(context.Background(), runtimes.ACPSlotSpec{
		ConnectionID: "conn_codex",
		RemoteID:     "local",
		SlotID:       "conn_codex:0",
		ProcessEpoch: "epoch_1",
	}, &runtimes.ACPPoolCapabilityReport{
		ConnectionID:     "conn_codex",
		WorkerResultHash: "worker_1",
		InitPhase:        runtimes.ACPPoolInitPhaseReady,
	})

	select {
	case <-pokes:
	case <-time.After(time.Second):
		t.Fatal("slot capability update did not poke remote status")
	}
	report, ok := reports.ACPPoolCapabilityReport(context.Background(), "conn_codex")
	require.True(t, ok)
	require.Equal(t, "worker_1", report.WorkerResultHash)
}
