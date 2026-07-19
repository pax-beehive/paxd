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
	"time"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/harnessregistry"
	"github.com/pax-beehive/paxd/internal/hostmetrics"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/pax-beehive/paxd/internal/localsessions"
	"github.com/pax-beehive/paxd/internal/sessionreporter"
)

const DefaultControlSocket = "~/.paxd/paxd.sock"

func DefaultControlSocketPath() string {
	return expandHome(DefaultControlSocket)
}

type Runtime struct {
	Store          *daemonstore.Store
	Control        control.Service
	LocalHandler   http.Handler
	supervisors    *runtimeSupervisors
	harnesses      control.HarnessRegistry
	hostMetrics    interface{ Start(context.Context) }
	sessionReports interface{ Start(context.Context) }
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
	supervisors := &runtimeSupervisors{
		statusHub:            newStatusHub(),
		paxdVersion:          opts.PaxdVersion,
		acpCapabilityReports: newACPCapabilityReports(),
	}
	service := control.NewService(control.ServiceOptions{
		Store:               store,
		Supervisors:         supervisors,
		Harnesses:           harnesses,
		LocalSessions:       localSessions,
		HostMetrics:         metrics,
		ACPPoolCapabilities: supervisors.acpCapabilityReports,
	})
	if err := supervisors.Configure(store, service); err != nil {
		return nil, fmt.Errorf("configure runtime supervisors: %w", err)
	}
	var reports interface{ Start(context.Context) }
	if supervisors.agentRuntimeSource != nil {
		reports = sessionreporter.New(sessionreporter.Options{
			RuntimeSource: supervisors.agentRuntimeSource,
			Scanner:       sessionreporter.DefaultScanner{},
			Reporter:      sessionreporter.CloudReporter{Headers: auth.NewProvider(store, nil)},
			BatchSize:     cfg.Daemon.SessionBatchSize,
		})
	}
	return &Runtime{
		Store:          store,
		Control:        service,
		LocalHandler:   localapi.NewHandler(service),
		supervisors:    supervisors,
		harnesses:      harnesses,
		hostMetrics:    metricsStarter,
		sessionReports: reports,
	}, nil
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
