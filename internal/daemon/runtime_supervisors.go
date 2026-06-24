package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/pax-beehive/paxd/internal/auth"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/controlws"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	runtimes "github.com/pax-beehive/paxd/internal/runtime"
	"github.com/pax-beehive/paxd/internal/supervisor"
	"github.com/pax-beehive/paxkit/reliablemq/sqlstore"
)

type runtimeSupervisors struct {
	remote *supervisor.RemoteSupervisor
	agent  *supervisor.AgentConnectionSupervisor
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

	headers := auth.NewProvider(store, nil)
	dialer := runtimes.GorillaWebSocketDialer{}
	runner := controlws.NewRunner(service)

	s.remote = supervisor.NewRemoteSupervisor(supervisor.RemoteSupervisorOptions{
		Store:   store,
		Factory: remoteControlSessionFactory{headers: headers, dialer: dialer, runner: runner},
	})
	s.agent = supervisor.NewAgentConnectionSupervisor(supervisor.AgentConnectionSupervisorOptions{
		Store: store,
		Factory: agentTunnelSessionFactory{deps: runtimes.AgentTunnelSessionDeps{
			Headers:               headers,
			Dialer:                dialer,
			ReliableEngineFactory: runtimes.ReliableEngineFromStore(mqStore),
		}},
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
	r.supervisors.Start(ctx)
}

func (s *runtimeSupervisors) Start(ctx context.Context) {
	if s == nil {
		log.Printf("[paxd] runtime supervisors are not configured")
		return
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
