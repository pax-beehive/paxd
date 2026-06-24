package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/harnessregistry"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/pax-beehive/paxd/internal/localsessions"
)

const DefaultControlSocket = "~/.paxd/paxd.sock"

type Runtime struct {
	Store        *daemonstore.Store
	Control      control.Service
	LocalHandler http.Handler
}

type Options struct {
	Config        *config.Config
	Store         *daemonstore.Store
	Harnesses     control.HarnessRegistry
	LocalSessions control.LocalSessions
	ImportYAML    bool
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
	if opts.ImportYAML {
		if err := ImportConfig(ctx, store, cfg); err != nil {
			return nil, err
		}
	}
	harnesses := opts.Harnesses
	if harnesses == nil {
		harnesses = harnessregistry.New(store)
	}
	localSessions := opts.LocalSessions
	if localSessions == nil {
		localSessions = localsessions.New(store, nil)
	}
	service := control.NewService(control.ServiceOptions{
		Store:         store,
		Harnesses:     harnesses,
		LocalSessions: localSessions,
	})
	return &Runtime{
		Store:        store,
		Control:      service,
		LocalHandler: localapi.NewHandler(service),
	}, nil
}

func ImportConfig(ctx context.Context, store *daemonstore.Store, cfg *config.Config) error {
	if store == nil {
		return errors.New("daemonstore is required")
	}
	remoteID := "default"
	remoteName := firstNonEmpty(cfg.Agent.Name, cfg.Agent.Hostname, "default")
	cloudURL := firstNonEmpty(cfg.Cloud.APIURL, cfg.Cloud.URL)
	if cloudURL != "" {
		remote := control.Remote{
			ID:          remoteID,
			Name:        remoteName,
			CloudAPIURL: cloudURL,
			NodeID:      cfg.Cloud.NodeID,
			Enabled:     boolPtr(true),
			IsDefault:   boolPtr(true),
		}
		if err := upsertRemote(ctx, store, remote, secretRef(cfg.Cloud.APIKey)); err != nil {
			return err
		}
		if err := syncRemoteAuth(ctx, store, remoteID, cfg); err != nil {
			return err
		}
	}
	for _, agent := range cfg.RuntimeAgents() {
		if agent.AgentID == "" && !runtimeAgentEnabled(agent) {
			continue
		}
		cmd := createConnectionCommand(remoteID, cfg, agent)
		if cmd.Name == "" || cmd.InstanceID == "" || cmd.AgentType == "" || len(cmd.Command) == 0 {
			continue
		}
		if err := upsertAgentConnection(ctx, store, cmd); err != nil {
			return err
		}
	}
	return nil
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

func upsertRemote(ctx context.Context, store *daemonstore.Store, remote control.Remote, cloudAPIKeyRef string) error {
	existing, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return err
	}
	for _, item := range existing {
		if item.Remote.ID != remote.ID {
			continue
		}
		patch := control.RemotePatch{}
		changed := false
		if item.Remote.Name != remote.Name {
			patch.Name = &remote.Name
			changed = true
		}
		if item.Remote.CloudAPIURL != remote.CloudAPIURL {
			patch.CloudAPIURL = &remote.CloudAPIURL
			changed = true
		}
		if item.Remote.NodeID != remote.NodeID {
			patch.NodeID = &remote.NodeID
			changed = true
		}
		if boolValue(item.Remote.Enabled) != boolValue(remote.Enabled) {
			patch.Enabled = remote.Enabled
			changed = true
		}
		if boolValue(item.Remote.IsDefault) != boolValue(remote.IsDefault) {
			patch.IsDefault = remote.IsDefault
			changed = true
		}
		material, materialErr := store.GetRemoteAuthMaterial(ctx, remote.ID)
		cloudAPIKeyRefPtr := (*string)(nil)
		if materialErr == nil && material.CloudAPIKeyRef != cloudAPIKeyRef {
			cloudAPIKeyRefPtr = &cloudAPIKeyRef
			changed = true
		}
		if materialErr != nil && cloudAPIKeyRef != "" {
			cloudAPIKeyRefPtr = &cloudAPIKeyRef
			changed = true
		}
		if changed {
			_, err := store.UpdateRemote(ctx, control.UpdateRemoteCommand{
				RemoteID:       remote.ID,
				Remote:         patch,
				CloudAPIKeyRef: cloudAPIKeyRefPtr,
			})
			return err
		}
		return nil
	}
	_, err = store.CreateRemote(ctx, control.CreateRemoteCommand{
		Remote:         remote,
		CloudAPIKeyRef: cloudAPIKeyRef,
	})
	return err
}

func syncRemoteAuth(ctx context.Context, store *daemonstore.Store, remoteID string, cfg *config.Config) error {
	if cfg.Cloud.CFClientID == "" && cfg.Cloud.CFClientSecret == "" {
		if err := store.ClearRemoteAuth(ctx, control.ClearRemoteAuthCommand{RemoteID: remoteID}); err != nil {
			return fmt.Errorf("clear remote auth: %w", err)
		}
		return nil
	}
	if cfg.Cloud.CFClientID == "" || cfg.Cloud.CFClientSecret == "" {
		return fmt.Errorf("import remote auth: cloudflare access requires both client id and client secret")
	}
	cmd := control.ConfigureRemoteAuthCommand{
		RemoteID: remoteID,
		Kind:     control.RemoteAuthCloudflareAccess,
		CloudflareAccess: &control.CloudflareAccessAuth{
			ClientID:        cfg.Cloud.CFClientID,
			ClientSecretRef: secretRef(cfg.Cloud.CFClientSecret),
		},
	}
	if err := cmd.Validate(); err != nil {
		return fmt.Errorf("import remote auth: %w", err)
	}
	if err := store.ConfigureRemoteAuth(ctx, cmd); err != nil {
		return fmt.Errorf("import remote auth: %w", err)
	}
	return nil
}

func upsertAgentConnection(ctx context.Context, store *daemonstore.Store, cmd control.CreateAgentConnectionCommand) error {
	existing, err := store.ListAgentConnections(ctx, control.ListAgentConnectionsQuery{IncludeDisabled: true})
	if err != nil {
		return err
	}
	for _, item := range existing {
		if item.ID != cmd.ID {
			continue
		}
		if agentConnectionMatches(item, cmd) {
			return nil
		}
		desired := cmd.DesiredState
		update := control.UpdateAgentConnectionCommand{
			ConnectionID: cmd.ID,
			Name:         &cmd.Name,
			CloudAgentID: &cmd.CloudAgentID,
			InstanceID:   &cmd.InstanceID,
			AgentType:    &cmd.AgentType,
			Harness:      &cmd.Harness,
			Command:      &cmd.Command,
			WorkingDir:   &cmd.WorkingDir,
			TunnelPath:   &cmd.TunnelPath,
			Env:          &cmd.Env,
			Enabled:      cmd.Enabled,
			DesiredState: &desired,
		}
		_, err := store.UpdateAgentConnection(ctx, update)
		return err
	}
	_, err = store.CreateAgentConnection(ctx, cmd)
	return err
}

func agentConnectionMatches(item control.AgentConnectionView, cmd control.CreateAgentConnectionCommand) bool {
	return item.RemoteID == cmd.RemoteID &&
		item.Name == cmd.Name &&
		item.CloudAgentID == cmd.CloudAgentID &&
		item.InstanceID == cmd.InstanceID &&
		item.AgentType == cmd.AgentType &&
		item.Harness == cmd.Harness &&
		stringSlicesEqual(item.Command, cmd.Command) &&
		item.WorkingDir == cmd.WorkingDir &&
		item.TunnelPath == cmd.TunnelPath &&
		stringMapsEqual(item.Env, cmd.Env) &&
		item.Enabled == boolValue(cmd.Enabled) &&
		item.DesiredState == cmd.DesiredState
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func createConnectionCommand(remoteID string, cfg *config.Config, agent config.RuntimeAgentConfig) control.CreateAgentConnectionCommand {
	enabled := runtimeAgentEnabled(agent)
	desired := control.DesiredStateRunning
	if !enabled {
		desired = control.DesiredStateStopped
	}
	harness := normalizeHarness(firstNonEmpty(agent.ACPForwarder.Harness, cfg.ACPForwarder.Harness, agent.AgentType))
	command := cfg.ACPForwarder.Command
	if len(agent.ACPForwarder.Command) > 0 {
		command = agent.ACPForwarder.Command
	}
	if len(command) == 0 {
		command = acpCommandForHarness(harness)
	}
	instanceID := firstNonEmpty(agent.InstanceID, agent.AgentID, agent.Name)
	name := firstNonEmpty(agent.Name, instanceID, agent.AgentID)
	return control.CreateAgentConnectionCommand{
		ID:           instanceID,
		RemoteID:     remoteID,
		Name:         name,
		CloudAgentID: agent.AgentID,
		InstanceID:   instanceID,
		AgentType:    firstNonEmpty(agent.AgentType, harness),
		Harness:      firstNonEmpty(harness, agent.AgentType),
		Command:      append([]string(nil), command...),
		WorkingDir:   firstNonEmpty(agent.ACPForwarder.WorkingDir, cfg.ACPForwarder.WorkingDir),
		TunnelPath:   firstNonEmpty(agent.ACPForwarder.TunnelPath, cfg.ACPForwarder.TunnelPath, "/api/v1/agent/tunnel"),
		Enabled:      &enabled,
		DesiredState: desired,
	}
}

func acpCommandForHarness(harness string) []string {
	switch normalizeHarness(harness) {
	case "hermes":
		return []string{"hermes", "acp"}
	case "codex":
		return []string{"codex", "--acp"}
	case "claude", "claude-code":
		return []string{"claude", "--acp"}
	case "gemini":
		return []string{"gemini", "--acp"}
	default:
		return nil
	}
}

func normalizeHarness(harness string) string {
	harness = strings.ToLower(strings.TrimSpace(harness))
	return strings.ReplaceAll(harness, "_", "-")
}

func runtimeAgentEnabled(agent config.RuntimeAgentConfig) bool {
	return agent.Enabled == nil || *agent.Enabled
}

func secretRef(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if strings.Contains(value, ":") {
		return value
	}
	return "inline:" + value
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func boolPtr(value bool) *bool {
	return &value
}

func boolValue(value *bool) bool {
	return value != nil && *value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
