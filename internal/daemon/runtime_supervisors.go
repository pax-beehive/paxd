package daemon

import (
	"context"
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
	remote             supervisor.Supervisor
	agent              supervisor.Supervisor
	agentRuntimeSource *supervisor.AgentConnectionSupervisor
	statusHub          *statusHub
	paxdVersion        string
	transportFlusher   interface {
		Close(context.Context) error
		Stats() reliablemq.ProducerWriteBehindStats
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
	agentProcessPool := runtimes.NewPersistentACPProcessPool(
		runtimes.ExecLocalACPProcessRunner{},
		transportStore,
		runtimes.WithPaxdVersionProvider(runtimes.StaticPaxdVersionProvider(s.paxdVersion)),
	)

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
			ACPProcessPool:        agentProcessPool,
			ReliableEngineFactory: runtimes.ReliableEngineFromStore(transportStore),
			TransportReconciler:   transportStore,
		}},
		StopHandler: func(ctx context.Context, spec runtimes.AgentConnectionSpec) {
			agentProcessPool.Stop(spec)
		},
	})
	s.agent = agent
	s.agentRuntimeSource = agent
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
	if s == nil || s.agent == nil {
		log.Printf("[paxd] agent connection supervisor wake requested before supervisor is configured")
		return
	}
	s.agent.Wake()
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
