package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/controlws"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/e2ee"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/secretchannel"
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
	attachmentStates     *attachmentStateHub
	paxdVersion          string
	bootID               string
	acpCapabilityReports *acpCapabilityReports
	maintenance          *lifecycleCoordinator
	transportDB          *sql.DB
	transportPruner      transportJournalPruner
	transportGCConfig    transportGCConfig
	transportGCDone      chan struct{}
	transportGCMu        sync.Mutex
	transportGCStats     transportGCStats
	transportFlusher     interface {
		Close(context.Context) error
		Stats() reliablemq.ProducerWriteBehindStats
	}
	transportProducers interface {
		Close(context.Context) error
	}
	transportStats      *runtimes.TransportStatsTracker
	startedAt           time.Time
	logFilePath         string
	store               *daemonstore.Store
	acpPoolRegistry     *runtimes.ACPPoolRegistry
	e2eeRootKey         []byte
	e2eeRootKeyProvider e2ee.RootKeyProvider
	lifecycleMu         sync.Mutex
	lifecycleCancel     context.CancelFunc
	lifecycleDone       chan struct{}
	lifecycleErr        error
}

type transportJournalPruner interface {
	PruneAckedOutbound(
		context.Context,
		sqlstore.AckedOutboundPruneOptions,
	) (sqlstore.PruneResult, error)
}

type transportGCConfig struct {
	Interval   time.Duration
	KeepFor    time.Duration
	KeepLatest int
	BatchSize  int
	Now        func() time.Time
}

type transportGCStats struct {
	lastRunAt    time.Time
	lastDeleted  int64
	totalDeleted int64
	lastError    string
}

func (s *runtimeSupervisors) Configure(store *daemonstore.Store, service control.Service) error {
	if store == nil {
		return errors.New("daemon store is required")
	}
	if service == nil {
		return errors.New("control service is required")
	}
	s.store = store
	sqlDB, err := openTransportDB()
	if err != nil {
		return fmt.Errorf("open reliable transport database: %w", err)
	}
	mqStore, err := sqlstore.NewSQLite(sqlDB, sqlstore.WithTableName("transport_journal"))
	if err != nil {
		_ = sqlDB.Close()
		return fmt.Errorf("open reliable transport journal: %w", err)
	}
	s.transportDB = sqlDB
	s.transportPruner = mqStore
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
	engineFactory := runtimes.NewTransportStatsTracker(runtimes.ReliableEngineFromStoreWithProducerConfig(
		transportStore,
		reliablemq.ProducerConfig{OnError: func(err error) {
			log.Printf("[paxd] FATAL acp transport producer stopped accepting output: %v", err)
		}},
	))
	s.transportStats = engineFactory
	s.transportProducers = engineFactory

	headers := auth.NewProvider(store, nil)
	dialer := runtimes.GorillaWebSocketDialer{}
	runner := controlws.NewRunner(service)
	runner.Reports = controlws.ReportOptions{
		HeartbeatInterval:    10 * time.Second,
		SendInitialHeartbeat: true,
		Heartbeat: func() control.HeartbeatReport {
			return control.HeartbeatReport{
				BootID: s.bootID, PaxdVersion: s.paxdVersion, DaemonPhase: s.daemonPhase(),
			}
		},
		SnapshotInterval:                  30 * time.Second,
		SendInitialSnapshot:               true,
		PokeDebounce:                      500 * time.Millisecond,
		SessionRuntimeSnapshotInterval:    30 * time.Second,
		SendInitialSessionRuntimeSnapshot: true,
	}
	if s.statusHub != nil {
		runner.Reports.StatusSubscribe = s.statusHub.Subscribe
	}
	if s.attachmentStates != nil {
		runner.Reports.AttachmentSubscribe = s.attachmentStates.Subscribe
	}
	acpPoolRegistry := runtimes.NewACPPoolRegistry(runtimes.ACPRouteStoreFactoryFunc(func(connectionID string) runtimes.ACPRouteStore {
		return acpRouteStoreAdapter{store: store}
	}))
	s.acpPoolRegistry = acpPoolRegistry
	acpPoolRegistry.SetBootID(s.bootID)

	remoteFactory := &remoteControlSessionFactory{headers: headers, dialer: dialer, runner: runner}
	remote := supervisor.NewRemoteSupervisor(supervisor.RemoteSupervisorOptions{
		Store:      store,
		Factory:    remoteFactory,
		StatusPoke: s.statusHub,
	})
	remoteFactory.eventSink = remote
	s.remote = remote
	agentFactory := &agentTunnelSessionFactory{deps: runtimes.AgentTunnelSessionDeps{
		Headers:               headers,
		Dialer:                dialer,
		ACPPoolRegistry:       acpPoolRegistry,
		ReliableEngineFactory: engineFactory,
		E2EERootKey:           append([]byte(nil), s.e2eeRootKey...),
		E2EERootKeyProvider:   s.e2eeRootKeyProvider,
		E2EECommandStore:      store,
		ACPSessionBindings:    acpRouteStoreAdapter{store: store},
	}}
	agent := supervisor.NewAgentConnectionSupervisor(supervisor.AgentConnectionSupervisorOptions{
		Store:      store,
		StatusPoke: s.statusHub,
		Factory:    agentFactory,
	})
	agentFactory.deps.SessionEventSink = agent
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

func (s *runtimeSupervisors) daemonPhase() string {
	if s == nil || s.maintenance == nil {
		return "running"
	}
	return s.maintenance.DaemonPhase()
}

func (s *runtimeSupervisors) BuildSessionRuntimeSnapshots(
	ctx context.Context,
	remoteID string,
	nodeID string,
) ([]control.SessionRuntimeSnapshotReport, error) {
	if s == nil || s.store == nil || s.acpPoolRegistry == nil {
		return nil, nil
	}
	connections, err := s.store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{
		RemoteID: remoteID, IncludeDisabled: true,
	})
	if err != nil {
		return nil, err
	}
	reports := make([]control.SessionRuntimeSnapshotReport, 0, len(connections))
	for _, connection := range connections {
		if strings.TrimSpace(connection.CloudAgentID) == "" {
			continue
		}
		pool, err := s.acpPoolRegistry.Get(connection.ID)
		if err != nil {
			return nil, err
		}
		snapshot := pool.RuntimeSnapshot()
		report := control.SessionRuntimeSnapshotReport{
			AgentID: connection.CloudAgentID, ConnectionID: connection.ID,
			SchemaVersion: 1,
			ActiveTurns:   make([]control.SessionActiveTurnReport, 0, len(snapshot.ActiveTurns)),
		}
		for _, turn := range snapshot.ActiveTurns {
			report.ActiveTurns = append(report.ActiveTurns, control.SessionActiveTurnReport{
				NativeSessionID: turn.NativeSessionID, TurnID: turn.TurnID, TurnInstanceID: turn.TurnID,
				PromptRequestID:   append(json.RawMessage(nil), turn.PromptRequestID...),
				RuntimeStatus:     string(turn.RuntimeStatus),
				PendingApprovalID: append(json.RawMessage(nil), turn.PendingApprovalID...),
				SlotID:            turn.SlotID, ProcessEpoch: turn.ProcessEpoch,
			})
		}
		reports = append(reports, report)
	}
	return reports, nil
}

func (s *runtimeSupervisors) SubscribeSessionRuntime(remoteID string) (<-chan struct{}, func()) {
	if s == nil || s.acpPoolRegistry == nil {
		return nil, func() {}
	}
	return s.acpPoolRegistry.SubscribeSessionRuntime(remoteID)
}

func (s *runtimeSupervisors) ResetSessionRuntime(
	ctx context.Context,
	remoteID string,
	command control.ResetSessionRuntimeCommand,
) (control.SessionRuntimeResetResult, error) {
	if s == nil || s.store == nil || s.acpPoolRegistry == nil {
		return control.SessionRuntimeResetResult{}, control.ControlError{
			Code: control.ErrCodeInternal, Message: "session runtime source is not configured",
		}
	}
	connections, err := s.store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{
		RemoteID: remoteID, IncludeDisabled: true,
	})
	if err != nil {
		return control.SessionRuntimeResetResult{}, err
	}
	authorized := false
	for _, connection := range connections {
		if connection.ID == command.ConnectionID && connection.CloudAgentID == command.AgentID {
			authorized = true
			break
		}
	}
	if !authorized {
		return control.SessionRuntimeResetResult{}, control.ControlError{
			Code: control.ErrCodeNotFound, Message: "agent connection is not bound to this remote",
		}
	}
	result := s.acpPoolRegistry.ResetSessionRuntime(
		command.ConnectionID,
		command.NativeSessionID,
		command.ExpectedTurnInstanceID,
	)
	return control.SessionRuntimeResetResult{
		Status: string(result.Status), ProjectionRevision: result.Revision,
	}, nil
}

func openTransportDB() (*sql.DB, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	path := filepath.Join(home, ".paxd", "transport.db")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create transport database directory: %w", err)
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	db, err := sql.Open("sqlite3", path+separator+"_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	log.Printf("[paxd] reliable transport database opened path=%s", path)
	return db, nil
}

func (s *runtimeSupervisors) RuntimeDiagnostics(ctx context.Context) control.RuntimeDiagnostics {
	_ = ctx
	out := control.RuntimeDiagnostics{PaxdVersion: s.paxdVersion}
	if !s.startedAt.IsZero() {
		startedAt := s.startedAt
		out.StartedAt = &startedAt
	}
	if s.logFilePath != "" {
		info := control.LogFileInfo{Path: s.logFilePath}
		if stat, err := os.Stat(s.logFilePath); err == nil {
			info.SizeBytes = stat.Size()
		}
		out.LogFile = &info
	}
	if s.transportStats != nil {
		stats := s.transportStats.Stats()
		queueIDs := make([]string, 0, len(stats))
		for queueID := range stats {
			queueIDs = append(queueIDs, queueID)
		}
		sort.Strings(queueIDs)
		for _, queueID := range queueIDs {
			stat := stats[queueID]
			out.TransportQueues = append(out.TransportQueues, control.TransportQueueDiagnostics{
				QueueID:      queueID,
				Bound:        stat.Bound,
				Tail:         stat.Tail,
				AckedThrough: stat.AckedThrough,
				NextToSend:   stat.NextToSend,
				Unacked:      stat.Tail - stat.AckedThrough,
				LastError:    stat.LastError,
			})
		}
	}
	if s.transportFlusher != nil {
		stats := s.transportFlusher.Stats()
		out.TransportWriteBehind = &control.TransportWriteBehindStats{
			DirtyFrames:              stats.DirtyFrames,
			DirtyPatches:             stats.DirtyPatches,
			DirtyBytes:               stats.DirtyBytes,
			Degraded:                 stats.Degraded,
			ConsecutiveFlushFailures: stats.ConsecutiveFlushFailures,
			LastFlushError:           stats.LastFlushError,
		}
	}
	s.transportGCMu.Lock()
	gcStats := s.transportGCStats
	s.transportGCMu.Unlock()
	if !gcStats.lastRunAt.IsZero() || gcStats.totalDeleted > 0 || gcStats.lastError != "" {
		lastRunAt := gcStats.lastRunAt
		out.TransportJournalGC = &control.TransportJournalGCStats{
			LastRunAt:    &lastRunAt,
			LastDeleted:  gcStats.lastDeleted,
			TotalDeleted: gcStats.totalDeleted,
			LastError:    gcStats.lastError,
		}
	}
	return out
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
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	if r.lifecycleStarted {
		r.lifecycleMu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.lifecycleStarted = true
	r.lifecycleCancel = cancel
	r.lifecycleMu.Unlock()

	if r.artifactJobs != nil {
		r.artifactJobs.Start(runCtx)
	}
	if r.supervisors == nil {
		log.Printf("[paxd] runtime supervisors are not configured")
		return
	}
	if r.hostMetrics != nil {
		r.hostMetrics.Start(runCtx)
	}
	startHarnessRefresh(runCtx, r.harnesses)
	if r.sessionReports != nil {
		r.sessionReports.Start(runCtx)
	}
	startSecretChannelSweep(runCtx, r.secretChannel, r.secretDrop)
	r.supervisors.Start(runCtx)
}

// startSecretChannelSweep is the periodic safety net for
// internal/secretchannel: Open/Consume already enforce TTLs inline, and
// FileDrop.Write's own TTL return value tells a well-behaved consumer when
// to stop trusting a file, but nothing else proactively deletes an
// abandoned in-memory channel or an unclaimed transient file until this
// runs.
func startSecretChannelSweep(ctx context.Context, registry *secretchannel.Registry, drop secretchannel.FileDrop) {
	if registry == nil {
		return
	}
	go func() {
		sweep := func() {
			registry.Sweep()
			if _, err := drop.Sweep(); err != nil && ctx.Err() == nil {
				log.Printf("[paxd] secret channel: transient file sweep failed: %v", err)
			}
		}
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.lifecycleMu.Lock()
	cancel := r.lifecycleCancel
	supervisors := r.supervisors
	r.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if supervisors == nil {
		return nil
	}
	return supervisors.Shutdown(ctx)
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
	s.lifecycleMu.Lock()
	if s.lifecycleDone != nil {
		s.lifecycleMu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.lifecycleCancel = cancel
	s.lifecycleDone = done
	s.lifecycleMu.Unlock()

	s.startTransportGC(runCtx)
	var supervisors sync.WaitGroup
	startSupervisor(runCtx, "remote", s.remote, &supervisors)
	startSupervisor(runCtx, "agent_connection", s.agent, &supervisors)
	startSupervisor(runCtx, "acp_slot", s.acpSlots, &supervisors)
	go func() {
		<-runCtx.Done()
		supervisors.Wait()
		shutdownErr := s.closeTransport()
		s.lifecycleMu.Lock()
		s.lifecycleErr = shutdownErr
		close(done)
		s.lifecycleMu.Unlock()
	}()
}

func (s *runtimeSupervisors) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.lifecycleMu.Lock()
	cancel := s.lifecycleCancel
	done := s.lifecycleDone
	s.lifecycleMu.Unlock()
	if done == nil {
		return nil
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
		s.lifecycleMu.Lock()
		err := s.lifecycleErr
		s.lifecycleMu.Unlock()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *runtimeSupervisors) closeTransport() error {
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result error
	if s.transportGCDone != nil {
		select {
		case <-s.transportGCDone:
		case <-closeCtx.Done():
			err := errors.New("timed out waiting for transport journal GC to stop")
			log.Printf("[paxd] %v", err)
			result = errors.Join(result, err)
		}
	}
	if s.transportProducers != nil {
		if err := s.transportProducers.Close(closeCtx); err != nil {
			log.Printf("[paxd] acp transport producer registry close failed: %v", err)
			result = errors.Join(result, fmt.Errorf("close transport producer registry: %w", err))
		}
	}
	if s.transportFlusher != nil {
		if err := s.transportFlusher.Close(closeCtx); err != nil {
			stats := s.transportFlusher.Stats()
			log.Printf(
				"[paxd] acp transport producer write-behind close failed: %v dirty_frames=%d dirty_patches=%d dirty_bytes=%d",
				err,
				stats.DirtyFrames,
				stats.DirtyPatches,
				stats.DirtyBytes,
			)
			result = errors.Join(result, fmt.Errorf("close transport flusher: %w", err))
		}
	}
	if s.transportDB != nil {
		if err := s.transportDB.Close(); err != nil {
			log.Printf("[paxd] close reliable transport database failed: %v", err)
			result = errors.Join(result, fmt.Errorf("close transport database: %w", err))
		}
	}
	return result
}

func (s *runtimeSupervisors) startTransportGC(ctx context.Context) {
	if s == nil || s.transportPruner == nil || s.transportGCConfig.Interval <= 0 ||
		s.transportGCConfig.KeepFor <= 0 || s.transportGCConfig.KeepLatest < 0 ||
		s.transportGCConfig.BatchSize <= 0 {
		return
	}
	if s.transportGCConfig.Now == nil {
		s.transportGCConfig.Now = time.Now
	}
	done := make(chan struct{})
	s.transportGCDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.transportGCConfig.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.pruneTransportJournal(ctx)
			}
		}
	}()
}

func (s *runtimeSupervisors) pruneTransportJournal(ctx context.Context) {
	now := s.transportGCConfig.Now().UTC()
	result, err := s.transportPruner.PruneAckedOutbound(ctx, sqlstore.AckedOutboundPruneOptions{
		OlderThan:          now.Add(-s.transportGCConfig.KeepFor),
		KeepLatestPerQueue: int64(s.transportGCConfig.KeepLatest),
		Limit:              s.transportGCConfig.BatchSize,
	})
	s.transportGCMu.Lock()
	s.transportGCStats.lastRunAt = now
	s.transportGCStats.lastDeleted = result.Deleted
	if err != nil {
		s.transportGCStats.lastError = err.Error()
	} else {
		s.transportGCStats.totalDeleted += result.Deleted
		s.transportGCStats.lastError = ""
	}
	s.transportGCMu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("[paxd] transport journal GC failed: %v", err)
		}
		return
	}
	if result.Deleted > 0 {
		log.Printf("[paxd] transport journal GC deleted=%d", result.Deleted)
	}
}

func startSupervisor(ctx context.Context, name string, sup supervisor.Supervisor, wg *sync.WaitGroup) {
	if sup == nil {
		log.Printf("[paxd] %s supervisor is not configured", name)
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := sup.Start(ctx)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("[paxd] %s supervisor exited: %v", name, err)
		}
	}()
}

type remoteControlSessionFactory struct {
	headers   auth.HeaderProvider
	dialer    runtimes.WebSocketDialer
	runner    runtimes.NodeControlRunner
	eventSink runtimes.SessionEventSink
}

func (f remoteControlSessionFactory) NewRemoteControlSession(spec runtimes.RemoteSpec) runtimes.Session {
	return runtimes.NewRemoteControlSession(runtimes.RemoteControlSessionConfig{
		Spec:             spec,
		Headers:          f.headers,
		Dialer:           f.dialer,
		Runner:           f.runner,
		NodeControlPath:  spec.NodeControlPath,
		SessionEventSink: f.eventSink,
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

func (a acpRouteStoreAdapter) NativeSessionID(ctx context.Context, connectionID string, managerSessionID string) (string, bool, error) {
	route, err := a.store.GetACPSessionRouteByManagerID(ctx, connectionID, managerSessionID)
	if errors.Is(err, daemonstore.ErrNotFound) {
		return "", false, nil
	}
	return route.NativeSessionID, err == nil, err
}

func (a acpRouteStoreAdapter) ManagerSessionID(ctx context.Context, connectionID string, nativeSessionID string) (string, bool, error) {
	route, err := a.store.GetACPSessionRoute(ctx, connectionID, nativeSessionID)
	if errors.Is(err, daemonstore.ErrNotFound) || (err == nil && route.ManagerSessionID == "") {
		return "", false, nil
	}
	return route.ManagerSessionID, err == nil, err
}

func (a acpRouteStoreAdapter) BindSessionIDs(ctx context.Context, connectionID string, managerSessionID string, nativeSessionID string) error {
	_, err := a.store.BindACPSessionRouteManagerID(ctx, daemonstore.ACPSessionManagerBinding{
		ConnectionID: connectionID, ManagerSessionID: managerSessionID, NativeSessionID: nativeSessionID,
	})
	return err
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
