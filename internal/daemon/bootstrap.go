package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/attachmentlocalizer"
	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/e2ee"
	"github.com/pax-beehive/paxd/internal/e2eepairing"
	"github.com/pax-beehive/paxd/internal/harnessregistry"
	"github.com/pax-beehive/paxd/internal/hostmetrics"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/pax-beehive/paxd/internal/localsessions"
	"github.com/pax-beehive/paxd/internal/sessionreporter"
	"github.com/pax-beehive/paxd/internal/updater"
)

const DefaultControlSocket = "~/.paxd/paxd.sock"

func DefaultControlSocketPath() string {
	return expandHome(DefaultControlSocket)
}

type Runtime struct {
	Store            *daemonstore.Store
	Control          control.Service
	LocalHandler     http.Handler
	supervisors      *runtimeSupervisors
	harnesses        control.HarnessRegistry
	hostMetrics      interface{ Start(context.Context) }
	sessionReports   interface{ Start(context.Context) }
	artifactJobs     interface{ Start(context.Context) }
	lifecycleMu      sync.Mutex
	lifecycleCancel  context.CancelFunc
	lifecycleStarted bool
	maintenance      *lifecycleCoordinator
}

type Options struct {
	Config        *config.Config
	Store         *daemonstore.Store
	Harnesses     control.HarnessRegistry
	LocalSessions control.LocalSessions
	HostMetrics   control.HostMetricsProvider
	PaxdVersion   string
}

func Bootstrap(ctx context.Context, opts Options) (*Runtime, error) {
	cfg := opts.Config
	if cfg == nil {
		return nil, errors.New("daemon config is required")
	}
	store := opts.Store
	if store == nil {
		opened, err := daemonstore.OpenSQLite(cfg.Daemon.DBPath)
		if err != nil {
			return nil, fmt.Errorf("open daemonstore: %w", err)
		}
		store = opened
	}
	if err := store.Migrate(ctx); err != nil {
		return nil, fmt.Errorf("migrate daemonstore: %w", err)
	}
	if _, err := store.ClearAllACPSessionRouteBindings(ctx); err != nil {
		return nil, fmt.Errorf("clear stale ACP session route bindings: %w", err)
	}
	harnesses := opts.Harnesses
	if harnesses == nil {
		harnesses = harnessregistry.New(store, harnessregistry.DefaultDetectors()...)
	}
	localSessions := opts.LocalSessions
	if localSessions == nil {
		localSessions = localsessions.New(store, nil)
	}
	metrics := opts.HostMetrics
	var metricsStarter interface{ Start(context.Context) }
	if metrics == nil {
		sampler := hostmetrics.NewSampler(10*time.Second).WithIdentity(
			cfg.Agent.MachineType,
			runtime.GOOS,
			runtime.GOARCH,
		)
		metrics = sampler
		metricsStarter = sampler
	}
	attachmentStates := newAttachmentStateHub()
	attachments := attachmentlocalizer.New(attachmentlocalizer.Options{
		Context: ctx,
		RootDir: cfg.Daemon.AttachmentDir,
		OnState: attachmentStates.Publish,
	})
	artifactJobs := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store,
		Targets: artifactpublisher.NewRemoteTargetResolver(
			store,
			auth.NewProvider(store, nil),
		),
		Manager:  artifactpublisher.NewHTTPManager(nil),
		Uploader: artifactpublisher.NewResumableUploader(nil),
	})
	bootID, err := newBootID()
	if err != nil {
		return nil, fmt.Errorf("generate daemon boot id: %w", err)
	}
	e2eeRootKey, err := loadE2EERootKeyFromEnvironment()
	if err != nil {
		return nil, err
	}
	var e2eeRootKeyProvider e2ee.RootKeyProvider
	var e2eeDistributionKeys e2ee.RootKeyProvider
	if len(e2eeRootKey) == 0 {
		nodeSeed, seedErr := e2ee.LoadOrCreateNodeSeed(
			ctx, e2eeNodeSeedPath(cfg.Daemon.DBPath), nil,
		)
		if seedErr != nil {
			return nil, fmt.Errorf("load E2EE node seed: %w", seedErr)
		}
		e2eeRootKeyProvider = e2ee.DerivedAgentRootKeyProvider{NodeSeed: nodeSeed}
		e2eeDistributionKeys = e2eeRootKeyProvider
	} else {
		e2eeDistributionKeys = e2ee.StaticRootKeyProvider{Key: e2eeRootKey}
	}
	maintenance := newLifecycleCoordinator(bootID)
	paxdUpdater := updater.New(updater.Options{
		CurrentVersion: opts.PaxdVersion,
		StateDir:       filepath.Join(filepath.Dir(cfg.Daemon.DBPath), "updates"),
	})
	artifactPublications := artifactpublisher.New(artifactpublisher.Options{
		Store:      store,
		RootDir:    cfg.Daemon.ArtifactSpoolDir,
		OnAccepted: artifactJobs.Wake,
	})
	supervisors := &runtimeSupervisors{
		statusHub:            newStatusHub(),
		attachmentStates:     attachmentStates,
		paxdVersion:          opts.PaxdVersion,
		bootID:               bootID,
		acpCapabilityReports: newACPCapabilityReports(),
		maintenance:          maintenance,
		startedAt:            time.Now().UTC(),
		logFilePath:          cfg.Daemon.LogFile,
		e2eeRootKey:          e2eeRootKey,
		e2eeRootKeyProvider:  e2eeRootKeyProvider,
		transportGCConfig: transportGCConfig{
			Interval:   cfg.Daemon.TransportJournalGCInterval,
			KeepFor:    cfg.Daemon.TransportJournalKeepAckedFor,
			KeepLatest: cfg.Daemon.TransportJournalKeepLatest,
			BatchSize:  cfg.Daemon.TransportJournalGCBatchSize,
			Now:        time.Now,
		},
	}
	service := control.NewService(control.ServiceOptions{
		Store:               store,
		Supervisors:         supervisors,
		Harnesses:           harnesses,
		LocalSessions:       localSessions,
		HostMetrics:         metrics,
		ACPPoolCapabilities: supervisors.acpCapabilityReports,
		Diagnostics:         supervisors,
		Attachments:         attachments,
		SessionRuntime:      supervisors,
		SessionRuntimeReset: supervisors,
		PaxdLifecycle:       maintenance,
	})
	if err := supervisors.Configure(store, service); err != nil {
		return nil, fmt.Errorf("configure runtime supervisors: %w", err)
	}
	var reports interface{ Start(context.Context) }
	maintenance.Configure(supervisors.acpPoolRegistry, paxdUpdater, store)
	if supervisors.agentRuntimeSource != nil {
		reports = sessionreporter.New(sessionreporter.Options{
			RuntimeSource: supervisors.agentRuntimeSource,
			Scanner:       sessionreporter.DefaultScanner{},
			Reporter:      sessionreporter.CloudReporter{Headers: auth.NewProvider(store, nil)},
			RouteResolver: sessionReportRouteResolver{store: store},
			BatchSize:     cfg.Daemon.SessionBatchSize,
		})
	}
	pairingService := e2eepairing.New(e2eepairing.ServiceOptions{
		Store: store, Headers: auth.NewProvider(store, nil), RootKeys: e2eeDistributionKeys,
	})
	return &Runtime{
		Store:   store,
		Control: service,
		LocalHandler: localapi.NewHandlerWithE2EE(artifactControlService{
			Service:      service,
			publications: artifactPublications,
		}, pairingService),
		supervisors:    supervisors,
		harnesses:      harnesses,
		hostMetrics:    metricsStarter,
		sessionReports: reports,
		artifactJobs:   artifactJobs,
		maintenance:    maintenance,
	}, nil
}

type sessionReportRouteResolver struct {
	store *daemonstore.Store
}

func (r sessionReportRouteResolver) ResolveManagerSessionID(
	ctx context.Context,
	connectionID string,
	nativeSessionID string,
) (string, bool, error) {
	route, err := r.store.GetACPSessionRoute(ctx, connectionID, nativeSessionID)
	if errors.Is(err, daemonstore.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return route.ManagerSessionID, true, nil
}

func loadE2EERootKeyFromEnvironment() ([]byte, error) {
	encoded := strings.TrimSpace(os.Getenv("PAX_E2EE_ROOT_KEY"))
	if encoded == "" {
		return nil, nil
	}
	rootKey, err := e2ee.ParseRootKey(encoded)
	if err != nil {
		return nil, fmt.Errorf("load E2EE root key from PAX_E2EE_ROOT_KEY: %w", err)
	}
	return rootKey, nil
}

func e2eeNodeSeedPath(databasePath string) string {
	return filepath.Join(filepath.Dir(databasePath), "secrets", "e2ee_node_seed")
}

type LocalAPIServer struct {
	listener net.Listener
	server   *http.Server
}

func StartUnixLocalAPI(ctx context.Context, socketPath string, handler http.Handler) (*LocalAPIServer, error) {
	socketPath = expandHome(firstNonEmpty(socketPath, DefaultControlSocket))
	if handler == nil {
		return nil, errors.New("local API handler is required")
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0700); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: handler}
	local := &LocalAPIServer{listener: listener, server: server}
	go func() {
		<-ctx.Done()
		_ = local.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = local.Close()
		}
	}()
	return local, nil
}

func StartDebugHTTP(ctx context.Context, addr string, handler http.Handler) (*LocalAPIServer, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("debug HTTP must bind to loopback, got %q", host)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: handler}
	local := &LocalAPIServer{listener: listener, server: server}
	go func() {
		<-ctx.Done()
		_ = local.Close()
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			_ = local.Close()
		}
	}()
	return local, nil
}

func (s *LocalAPIServer) Addr() net.Addr {
	if s == nil || s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *LocalAPIServer) Close() error {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s.server != nil {
		_ = s.server.Shutdown(ctx)
	}
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
