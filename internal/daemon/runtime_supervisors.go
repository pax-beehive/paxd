package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/controlws"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/pax-beehive/paxkit/reliablemq"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
)

type runtimeSupervisors struct {
	remote               supervisor.Supervisor
	agent                supervisor.Supervisor
	acpSlots             supervisor.Supervisor
	agentRuntimeSource   *supervisor.AgentConnectionSupervisor
	statusHub            *statusHub
	paxdVersion          string
	acpCapabilityReports *acpCapabilityReports
	transportFlusher     interface {
		Close(context.Context) error
		Stats() reliablemq.ProducerWriteBehindStats
	}
	transportProducers interface {
		Close(context.Context) error
	}
}

func (s *runtimeSupervisors) Configure(store *daemonstore.Store, service control.Service) error {
	if store == nil {
		return errors.New("daemon store is required")
	}
	if service == nil {
		return errors.New("control service is required")
	}
	sqlDB, err := store.DB().DB()
	if err != nil {
		return fmt.Errorf("get sqlite handle: %w", err)
	}
	mqStore, err := sqlstore.NewSQLite(sqlDB, sqlstore.WithTableName("transport_journal"))
	if err != nil {
		return fmt.Errorf("open reliable transport journal: %w", err)
	}
	transportStore := reliablemq.NewProducerWriteBehindStore(
		mqStore,
		reliablemq.WithProducerWriteBehindRequireBatchStore(),
		reliablemq.WithProducerWriteBehindFlushFailureHandler(func(err error, stats reliablemq.ProducerWriteBehindStats) {
			log.Printf(
				"[paxd] acp transport producer write-behind flush failed: %v dirty_frames=%d dirty_patches=%d dirty_bytes=%d consecutive_failures=%d last_error=%q",
				err,
				stats.DirtyFrames,
				stats.DirtyPatches,
				stats.DirtyBytes,
				stats.ConsecutiveFlushFailures,
				stats.LastFlushError,
			)
		}),
	)
	s.transportFlusher = transportStore
	engineFactory := runtimes.ReliableEngineFromStoreWithProducerConfig(
		transportStore,
		reliablemq.ProducerConfig{OnError: func(err error) {
			log.Printf("[paxd] FATAL acp transport producer stopped accepting output: %v", err)
		}},
	)
	if closer, ok := engineFactory.(interface{ Close(context.Context) error }); ok {
		s.transportProducers = closer
	}

	headers := auth.NewProvider(store, nil)
	dialer := runtimes.GorillaWebSocketDialer{}
	runner := controlws.NewRunner(service)
	runner.Reports = controlws.ReportOptions{
		HeartbeatInterval:   10 * time.Second,
		SnapshotInterval:    30 * time.Second,
		SendInitialSnapshot: true,
		PokeDebounce:        500 * time.Millisecond,
	}
	if s.statusHub != nil {
		runner.Reports.StatusSubscribe = s.statusHub.Subscribe
	}
	acpPoolRegistry := runtimes.NewACPPoolRegistry(runtimes.ACPRouteStoreFactoryFunc(func(connectionID string) runtimes.ACPRouteStore {
		return acpRouteStoreAdapter{store: store}
	}))

	s.remote = supervisor.NewRemoteSupervisor(supervisor.RemoteSupervisorOptions{
		Store:      store,
		Factory:    remoteControlSessionFactory{headers: headers, dialer: dialer, runner: runner},
		StatusPoke: s.statusHub,
	})
	agent := supervisor.NewAgentConnectionSupervisor(supervisor.AgentConnectionSupervisorOptions{
		Store:      store,
		StatusPoke: s.statusHub,
		Factory: agentTunnelSessionFactory{deps: runtimes.AgentTunnelSessionDeps{
			Headers:               headers,
			Dialer:                dialer,
			ACPPoolRegistry:       acpPoolRegistry,
			ReliableEngineFactory: engineFactory,
		}},
	})
	s.agent = agent
	s.agentRuntimeSource = agent
	s.acpSlots = supervisor.NewACPSlotSupervisor(supervisor.ACPSlotSupervisorOptions{
		Store: store,
		Factory: acpSlotSessionFactory{
			runner:            runtimes.ExecLocalACPProcessRunner{},
			registry:          acpPoolRegistry,
			paxdVersion:       s.paxdVersion,
			readyHandler:      acpSlotReadyStatusWriter(store),
			capabilityHandler: acpSlotCapabilityWriter(s.acpCapabilityReports, s.statusHub),
		},
		DrainHandler: func(ctx context.Context, spec runtimes.ACPSlotSpec) <-chan struct{} {
			_ = ctx
			pool, err := acpPoolRegistry.Get(spec.ConnectionID)
			if err != nil {
				log.Printf("[paxd] acp slot drain connection_id=%s slot_id=%s pool lookup failed: %v", spec.ConnectionID, spec.SlotID, err)
				return nil
			}
			return pool.BeginSlotDrain(spec.SlotID, spec.ProcessEpoch)
		},
		StopHandler: func(ctx context.Context, spec runtimes.ACPSlotSpec) {
			pool, err := acpPoolRegistry.Get(spec.ConnectionID)
			if err != nil {
				log.Printf("[paxd] acp slot stop connection_id=%s slot_id=%s pool lookup failed: %v", spec.ConnectionID, spec.SlotID, err)
				return
			}
			pool.RemoveSlot(ctx, spec.SlotID, spec.ProcessEpoch)
		},
	})
	return nil
}

func (s *runtimeSupervisors) WakeRemotes() {
	if s == nil || s.remote == nil {
		log.Printf("[paxd] remote supervisor wake requested before supervisor is configured")
		return
	}
	s.remote.Wake()
}

func (s *runtimeSupervisors) WakeAgentConnections() {
	if s == nil {
		log.Printf("[paxd] agent connection supervisor wake requested before supervisor is configured")
		return
	}
	if s.agent == nil {
		log.Printf("[paxd] agent connection supervisor wake requested before supervisor is configured")
	} else {
		s.agent.Wake()
	}

}

func (s *runtimeSupervisors) WakeACPSlots() {
	if s == nil {
		log.Printf("[paxd] acp slot supervisor wake requested before supervisor is configured")
		return
	}
	if s.acpSlots == nil {
		log.Printf("[paxd] acp slot supervisor wake requested before supervisor is configured")
	} else {
		s.acpSlots.Wake()
	}
}

func (r *Runtime) StartSupervisors(ctx context.Context) {
	if r == nil || r.supervisors == nil {
		log.Printf("[paxd] runtime supervisors are not configured")
		return
	}
	if r.hostMetrics != nil {
		r.hostMetrics.Start(ctx)
	}
	startHarnessRefresh(ctx, r.harnesses)
	if r.sessionReports != nil {
		r.sessionReports.Start(ctx)
	}
	r.supervisors.Start(ctx)
}

func startHarnessRefresh(ctx context.Context, harnesses control.HarnessRegistry) {
	if harnesses == nil {
		return
	}
	go func() {
		refresh := func() {
			if _, err := harnesses.Discover(ctx, control.DiscoverHarnessesQuery{}); err != nil && ctx.Err() == nil {
				log.Printf("[paxd] harness discovery refresh failed: %v", err)
			}
		}
		refresh()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func (s *runtimeSupervisors) Start(ctx context.Context) {
	if s == nil {
		log.Printf("[paxd] runtime supervisors are not configured")
		return
	}
	if s.transportFlusher != nil {
		go func() {
			<-ctx.Done()
			closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if s.transportProducers != nil {
				if err := s.transportProducers.Close(closeCtx); err != nil {
					log.Printf("[paxd] acp transport producer registry close failed: %v", err)
				}
			}
			if err := s.transportFlusher.Close(closeCtx); err != nil {
				stats := s.transportFlusher.Stats()
				log.Printf(
					"[paxd] acp transport producer write-behind close failed: %v dirty_frames=%d dirty_patches=%d dirty_bytes=%d",
					err,
					stats.DirtyFrames,
					stats.DirtyPatches,
					stats.DirtyBytes,
				)
			}
		}()
	}
	startSupervisor(ctx, "remote", s.remote)
	startSupervisor(ctx, "agent_connection", s.agent)
	startSupervisor(ctx, "acp_slot", s.acpSlots)
}

func startSupervisor(ctx context.Context, name string, sup supervisor.Supervisor) {
	if sup == nil {
		log.Printf("[paxd] %s supervisor is not configured", name)
		return
	}
	go func() {
		err := sup.Start(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[paxd] %s supervisor exited: %v", name, err)
		}
	}()
}

type remoteControlSessionFactory struct {
	headers auth.HeaderProvider
	dialer  runtimes.WebSocketDialer
	runner  runtimes.NodeControlRunner
}

func (f remoteControlSessionFactory) NewRemoteControlSession(spec runtimes.RemoteSpec) runtimes.Session {
	return runtimes.NewRemoteControlSession(runtimes.RemoteControlSessionConfig{
		Spec:            spec,
		Headers:         f.headers,
		Dialer:          f.dialer,
		Runner:          f.runner,
		NodeControlPath: spec.NodeControlPath,
	})
}

type agentTunnelSessionFactory struct {
	deps runtimes.AgentTunnelSessionDeps
}

func (f agentTunnelSessionFactory) NewAgentTunnelSession(spec runtimes.AgentConnectionSpec) runtimes.Session {
	return runtimes.NewAgentTunnelSession(spec, f.deps)
}

type acpSlotSessionFactory struct {
	runner            runtimes.LocalACPProcessRunner
	registry          *runtimes.ACPPoolRegistry
	paxdVersion       string
	readyHandler      func(context.Context, runtimes.ACPSlotSpec)
	capabilityHandler func(context.Context, runtimes.ACPSlotSpec, *runtimes.ACPPoolCapabilityReport)
}

func (f acpSlotSessionFactory) NewACPSlotSession(spec runtimes.ACPSlotSpec) runtimes.Session {
	spec.PaxdVersion = sFirstNonEmpty(spec.PaxdVersion, f.paxdVersion)
	return runtimes.NewACPSlotSession(runtimes.ACPSlotSessionConfig{
		Spec:              spec,
		Runner:            f.runner,
		Registry:          f.registry,
		ReadyHandler:      f.readyHandler,
		CapabilityHandler: f.capabilityHandler,
	})
}

func acpSlotCapabilityWriter(
	reports *acpCapabilityReports,
	status *statusHub,
) func(context.Context, runtimes.ACPSlotSpec, *runtimes.ACPPoolCapabilityReport) {
	return func(ctx context.Context, spec runtimes.ACPSlotSpec, report *runtimes.ACPPoolCapabilityReport) {
		if reports != nil {
			if err := reports.ReportACPSlotCapability(ctx, spec, report); err != nil {
				log.Printf(
					"[paxd] acp slot capability report failed connection_id=%s slot_id=%s process_epoch=%s: %v",
					spec.ConnectionID,
					spec.SlotID,
					spec.ProcessEpoch,
					err,
				)
			}
		}
		if status != nil {
			status.Poke(spec.RemoteID)
		}
	}
}

func acpSlotReadyStatusWriter(store *daemonstore.Store) func(context.Context, runtimes.ACPSlotSpec) {
	return func(ctx context.Context, spec runtimes.ACPSlotSpec) {
		readyAt := time.Now().UTC()
		if err := store.UpsertACPSlotStatus(ctx, daemonstore.ACPSlotStatusUpdate{
			SlotID:       spec.SlotID,
			ConnectionID: spec.ConnectionID,
			Ordinal:      spec.Ordinal,
			ProcessEpoch: spec.ProcessEpoch,
			Phase:        string(runtimes.ACPSlotPhaseReady),
			ReadyAt:      &readyAt,
		}); err != nil {
			log.Printf("[paxd] acp slot ready status write failed connection_id=%s slot_id=%s process_epoch=%s: %v", spec.ConnectionID, spec.SlotID, spec.ProcessEpoch, err)
		}
	}
}

type acpRouteStoreAdapter struct {
	store *daemonstore.Store
}

func (a acpRouteStoreAdapter) GetACPSessionRoute(ctx context.Context, connectionID string, nativeSessionID string) (runtimes.ACPRoute, bool, error) {
	route, err := a.store.GetACPSessionRoute(ctx, connectionID, nativeSessionID)
	if err != nil {
		if errors.Is(err, daemonstore.ErrNotFound) {
			return runtimes.ACPRoute{}, false, nil
		}
		return runtimes.ACPRoute{}, false, err
	}
	return runtimeACPRoute(route), true, nil
}

func (a acpRouteStoreAdapter) UpsertACPSessionRoute(ctx context.Context, connectionID string, nativeSessionID string, resumeParams json.RawMessage) (runtimes.ACPRoute, error) {
	route, err := a.store.UpsertACPSessionRoute(ctx, daemonstore.ACPSessionRouteUpsert{
		ConnectionID:     connectionID,
		NativeSessionID:  nativeSessionID,
		ResumeParamsJSON: string(resumeParams),
	})
	if err != nil {
		return runtimes.ACPRoute{}, err
	}
	return runtimeACPRoute(route), nil
}

func (a acpRouteStoreAdapter) BindACPSessionRoute(ctx context.Context, update runtimes.ACPRouteBindingUpdate) (runtimes.ACPRoute, bool, error) {
	route, ok, err := a.store.BindACPSessionRoute(ctx, daemonstore.ACPSessionRouteBindingUpdate{
		ConnectionID:     update.ConnectionID,
		NativeSessionID:  update.NativeSessionID,
		SlotID:           update.SlotID,
		ProcessEpoch:     update.ProcessEpoch,
		ExpectedVersion:  update.ExpectedVersion,
		ResumeParamsJSON: string(update.ResumeParams),
	})
	if err != nil || !ok {
		return runtimes.ACPRoute{}, ok, err
	}
	return runtimeACPRoute(route), true, nil
}

func (a acpRouteStoreAdapter) CountBoundACPSessionRoutesBySlot(ctx context.Context, connectionID string) (map[string]int, error) {
	return a.store.CountBoundACPSessionRoutesBySlot(ctx, connectionID)
}

func (a acpRouteStoreAdapter) ClearACPSessionRoutesForProcess(ctx context.Context, connectionID string, slotID string, processEpoch string) (int64, error) {
	return a.store.ClearACPSessionRoutesForProcess(ctx, connectionID, slotID, processEpoch)
}

func runtimeACPRoute(route daemonstore.ACPSessionRouteView) runtimes.ACPRoute {
	return runtimes.ACPRoute{
		ConnectionID:      route.ConnectionID,
		NativeSessionID:   route.NativeSessionID,
		BoundSlotID:       route.BoundSlotID,
		BoundProcessEpoch: route.BoundProcessEpoch,
		LastSlotID:        route.LastSlotID,
		ResumeParams:      json.RawMessage(route.ResumeParamsJSON),
		Version:           route.Version,
	}
}

func sFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
