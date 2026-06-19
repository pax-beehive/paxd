// paxd is the Pax Fleet Daemon — runs on every agent machine,
// connects to the Fleet Cloud API via WebSocket, reports local Hermes
// session status, and executes real-time messages from the Cloud mailbox.
//
// Architecture:
//
//	Cloud ──WebSocket──→ paxd (receive messages in real-time)
//	paxd  ──HTTP POST──→ Cloud (status reports, results, registration)
//
// Usage:
//
//	paxd register --cloud-url https://fleet.example.com    # first-time registration
//	paxd run                                                  # start the daemon loop
//	paxd install-service                                      # install as macOS launchd service
//	paxd --version                                            # print version
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/collector"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/executor"
	"github.com/pax-beehive/paxd/internal/hermes"
	"github.com/pax-beehive/paxd/internal/poller"
	"github.com/pax-beehive/paxd/internal/state"
	"github.com/pax-beehive/paxd/internal/store"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "connect":
		cmdConnect(os.Args[2:])
	case "register":
		cmdRegister(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "install-service":
		cmdInstallService()
	case "--version", "version":
		fmt.Println("paxd", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `paxd — Pax Fleet Daemon %s

Usage:
  paxd connect --cloud-url <url>      interactive device onboarding
  paxd register --cloud-url <url>     first-time registration
  paxd run                             start the daemon loop
  paxd install-service                 install as macOS launchd service
  paxd --version                       print version
`, version)
}

// cmdConnect performs interactive device-code style onboarding.
func cmdConnect(args []string) {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	cloudURL := fs.String("cloud-url", "", "Fleet Cloud API URL")
	configPath := fs.String("config", "", "Config file path (default: ~/.pax/paxd.yaml)")
	runAfter := fs.Bool("run", false, "Run paxd after successful connection")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	cloudAPIURL := firstNonEmpty(*cloudURL, cfg.Cloud.APIURL)
	if cloudAPIURL == "" {
		fmt.Fprintln(os.Stderr, "--cloud-url or cloud.api_url is required")
		os.Exit(1)
	}

	client := cloud.NewClient(cloudAPIURL, "")
	hostname := cfg.Agent.Hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	start, err := client.StartNodeRegistration(&cloud.StartNodeRegistrationRequest{
		Name:        cfg.Agent.Name,
		Hostname:    hostname,
		MachineType: cfg.Agent.MachineType,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		PaxdVersion: version,
		APIEndpoint: cfg.Hermes.APIEndpoint,
	})
	if err != nil {
		log.Fatalf("start registration: %v", err)
	}

	fmt.Println("Connect this machine to Pax:")
	fmt.Printf("\n  Open: %s\n", firstNonEmpty(start.VerificationURIComplete, start.VerificationURI))
	fmt.Printf("  Code: %s\n\n", start.PairCode)
	fmt.Println("Waiting for approval...")

	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for {
		if !deadline.IsZero() && time.Now().After(deadline) {
			log.Fatal("registration expired")
		}
		time.Sleep(interval)
		poll, err := client.PollNodeRegistration(&cloud.PollNodeRegistrationRequest{
			RegistrationID: start.RegistrationID,
			PollToken:      start.PollToken,
		})
		if err != nil {
			log.Fatalf("poll registration: %v", err)
		}
		switch poll.Status {
		case "pending":
			continue
		case "approved":
			if poll.NodeID == "" || poll.APIKey == "" {
				log.Fatal("registration approved without node credential")
			}
			configFile := saveNodeCredential(cfg, cloudAPIURL, poll.NodeID, poll.APIKey, *configPath)
			saveNodeState(cfg, cloudAPIURL, poll.NodeID, poll.APIKey)
			fmt.Printf("Connected successfully.\n")
			fmt.Printf("  node_id: %s\n", poll.NodeID)
			fmt.Printf("  config:  %s\n", configFile)
			if *runAfter {
				cmdRun([]string{"--config", configFile})
			}
			return
		case "slow_down":
			interval += time.Second
		case "denied", "expired":
			log.Fatalf("registration %s", poll.Status)
		default:
			log.Fatalf("registration returned unknown status %q", poll.Status)
		}
	}
}

// cmdRegister performs first-time registration with the Cloud API.
func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	cloudURL := fs.String("cloud-url", "", "Fleet Cloud API URL (required)")
	registrationToken := fs.String("registration-token", "", "Node registration token")
	configPath := fs.String("config", "", "Config file path (default: ~/.pax/paxd.yaml)")
	fs.Parse(args)

	if *cloudURL == "" {
		fmt.Fprintln(os.Stderr, "--cloud-url is required")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	token := firstNonEmpty(*registrationToken, cfg.Cloud.RegistrationToken)
	if token == "" {
		fmt.Fprintln(os.Stderr, "--registration-token or cloud.registration_token is required")
		os.Exit(1)
	}

	client := cloud.NewClient(*cloudURL, "")
	hostname, _ := os.Hostname()

	req := &cloud.RegisterNodeRequest{
		Name:        cfg.Agent.Name,
		Hostname:    hostname,
		MachineType: cfg.Agent.MachineType,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		PaxdVersion: version,
		APIEndpoint: cfg.Hermes.APIEndpoint,
	}

	resp, err := client.RegisterNode(req, token)
	if err != nil {
		log.Fatalf("register: %v", err)
	}

	configFile := saveNodeCredential(cfg, *cloudURL, resp.NodeID, resp.APIKey, *configPath)

	fmt.Printf("Registered successfully!\n")
	fmt.Printf("  node_id: %s\n", resp.NodeID)
	fmt.Printf("  config:  %s\n", configFile)
	fmt.Printf("\nRun: paxd run\n")
}

// cmdRun starts the main daemon loop.
//
// Three concurrent loops:
//  1. WebSocket → receives messages from Cloud in real-time
//  2. Status ticker → reports session + system status to Cloud (HTTP POST)
//  3. Orphan ticker → reconciles orphaned messages
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", "", "Config file path (default: ~/.pax/paxd.yaml)")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.Printf("[paxd] starting v%s on %s/%s", version, runtime.GOOS, runtime.GOARCH)

	// State machine
	sm := state.NewMachine()

	// Open local SQLite
	db, err := store.Open(cfg.Daemon.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Check if registered
	nodeState, err := db.GetNodeState()
	if err != nil {
		log.Fatalf("get node state: %v", err)
	}

	// Transition to REGISTERING
	if err := sm.Transition(state.REGISTERING); err != nil {
		log.Fatalf("state transition: %v", err)
	}

	if nodeState == nil {
		nodeState = nodeStateFromConfig(cfg, db)
	}

	if err := syncConfiguredAgents(cfg, db); err != nil {
		log.Fatalf("sync agents: %v", err)
	}

	// Create clients
	cloudURL := firstNonEmpty(nodeState.CloudAPIURL, cfg.Cloud.APIURL)
	cloudClient := cloud.NewClient(cloudURL, nodeState.CloudAPIKey)
	runtimes, executors, err := buildAgentRuntimes(cfg, cloudClient, db)
	if err != nil {
		log.Fatalf("build agent runtimes: %v", err)
	}
	if len(runtimes) == 0 {
		log.Fatal("no cloud agents configured. Set agent.agent_id, agents[].agent_id, or PAX_AGENT_ID")
	}

	// Check Hermes reachability
	for _, runtime := range runtimes {
		if err := runtime.HermesClient.Ping(); err != nil {
			log.Printf(
				"[paxd] WARNING: Hermes not reachable for agent %s: %v",
				runtime.Agent.AgentID,
				err,
			)
			log.Printf("[paxd] continuing; will retry on each status cycle")
		} else {
			log.Printf(
				"[paxd] Hermes reachable for agent %s at %s",
				runtime.Agent.AgentID,
				runtime.Agent.APIEndpoint,
			)
		}
	}

	// Create subsystems
	col := collector.New(runtimes, cloudClient, db, cfg.Agent.Hostname)
	pol := poller.New(cloudClient, nil, executors, db)

	// Transition to RUNNING
	if err := sm.Transition(state.RUNNING); err != nil {
		log.Fatalf("state transition: %v", err)
	}
	log.Printf("[paxd] RUNNING (node=%s, agents=%d, poll=%s, status=%s, orphan=%s)",
		nodeState.NodeID,
		len(runtimes),
		cfg.Daemon.PollInterval,
		cfg.Daemon.StatusInterval,
		cfg.Daemon.ReconcileInterval)

	// Main loop: mailbox polling, status reporting, and orphan reconciliation.
	pollTicker := time.NewTicker(cfg.Daemon.PollInterval)
	statusTicker := time.NewTicker(cfg.Daemon.StatusInterval)
	orphanTicker := time.NewTicker(cfg.Daemon.ReconcileInterval)
	defer pollTicker.Stop()
	defer statusTicker.Stop()
	defer orphanTicker.Stop()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case sig := <-sigCh:
			log.Printf("[paxd] received signal: %v", sig)
			sm.Transition(state.STOPPING)
			sm.Transition(state.STOPPED)
			log.Printf("[paxd] STOPPED")
			return

		case <-pollTicker.C:
			nextOffset, err := pol.PollMailbox(
				sm.Context(),
				nodeState.LastOffset,
				10,
			)
			if err != nil {
				log.Printf("[paxd] poll error: %v", err)
			}
			if nextOffset > nodeState.LastOffset {
				nodeState.LastOffset = nextOffset
			}

		case <-statusTicker.C:
			if err := col.CollectAndReport(sm.Context()); err != nil {
				log.Printf("[paxd] status error: %v", err)
			}

		case <-orphanTicker.C:
			if err := pol.ReconcileOrphans(sm.Context()); err != nil {
				log.Printf("[paxd] orphan reconcile error: %v", err)
			}
		}
	}
}

// wsURLFromHTTP derives the WebSocket URL from the HTTP Cloud URL.
// https://fleet.example.com → wss://fleet.example.com/api/agent/ws
func wsURLFromHTTP(httpURL string) string {
	u := strings.TrimRight(httpURL, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u + "/api/agent/ws"
}

func registerNodeFromConfig(cfg *config.Config, db *store.Store) *store.NodeState {
	if cfg.Cloud.APIURL == "" {
		log.Fatal("cloud.api_url or PAX_CLOUD_URL is required")
	}
	if cfg.Cloud.RegistrationToken == "" {
		log.Fatal("not registered. Run paxd register or set cloud.registration_token")
	}
	client := cloud.NewClient(cfg.Cloud.APIURL, "")
	req := &cloud.RegisterNodeRequest{
		Name:        cfg.Agent.Name,
		Hostname:    cfg.Agent.Hostname,
		MachineType: cfg.Agent.MachineType,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		PaxdVersion: version,
		APIEndpoint: cfg.Hermes.APIEndpoint,
	}
	resp, err := client.RegisterNode(req, cfg.Cloud.RegistrationToken)
	if err != nil {
		log.Fatalf("register node: %v", err)
	}
	nodeState := &store.NodeState{
		NodeID:       resp.NodeID,
		CloudAPIKey:  resp.APIKey,
		CloudAPIURL:  cfg.Cloud.APIURL,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := db.SaveNodeState(nodeState); err != nil {
		log.Fatalf("save node state: %v", err)
	}
	log.Printf("[paxd] registered as node %s", resp.NodeID)
	return nodeState
}

func nodeStateFromConfig(cfg *config.Config, db *store.Store) *store.NodeState {
	if cfg.Cloud.APIKey != "" && cfg.Cloud.NodeID != "" {
		nodeState := &store.NodeState{
			NodeID:       cfg.Cloud.NodeID,
			CloudAPIKey:  cfg.Cloud.APIKey,
			CloudAPIURL:  cfg.Cloud.APIURL,
			RegisteredAt: time.Now().UTC().Format(time.RFC3339),
		}
		if err := db.SaveNodeState(nodeState); err != nil {
			log.Fatalf("save node state: %v", err)
		}
		return nodeState
	}
	return registerNodeFromConfig(cfg, db)
}

func saveNodeCredential(
	cfg *config.Config,
	cloudURL string,
	nodeID string,
	apiKey string,
	configPath string,
) string {
	cfg.Cloud.APIURL = cloudURL
	cfg.Cloud.NodeID = nodeID
	cfg.Cloud.APIKey = apiKey
	cfg.Cloud.RegistrationToken = ""

	configFile := configPath
	if configFile == "" {
		home, _ := os.UserHomeDir()
		configFile = filepath.Join(home, ".pax", "paxd.yaml")
	}
	if err := os.MkdirAll(filepath.Dir(configFile), 0700); err != nil {
		log.Fatalf("create config dir: %v", err)
	}
	data, err := yamlMarshal(cfg)
	if err != nil {
		log.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		log.Fatalf("write config: %v", err)
	}
	return configFile
}

func saveNodeState(cfg *config.Config, cloudURL string, nodeID string, apiKey string) {
	db, err := store.Open(cfg.Daemon.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := db.SaveNodeState(&store.NodeState{
		NodeID:       nodeID,
		CloudAPIKey:  apiKey,
		CloudAPIURL:  cloudURL,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.Fatalf("save node state: %v", err)
	}
}

func syncConfiguredAgents(cfg *config.Config, db *store.Store) error {
	for _, runtimeAgent := range cfg.RuntimeAgents() {
		if runtimeAgent.AgentID == "" {
			continue
		}
		enabled := true
		if runtimeAgent.Enabled != nil {
			enabled = *runtimeAgent.Enabled
		}
		agent := &store.CloudAgent{
			AgentID:     runtimeAgent.AgentID,
			InstanceID:  firstNonEmpty(runtimeAgent.InstanceID, runtimeAgent.AgentID),
			Name:        firstNonEmpty(runtimeAgent.Name, "hermes"),
			AgentType:   firstNonEmpty(runtimeAgent.AgentType, "hermes"),
			APIEndpoint: firstNonEmpty(runtimeAgent.APIEndpoint, cfg.Hermes.APIEndpoint),
			APIKeyEnv:   firstNonEmpty(runtimeAgent.APIKeyEnv, cfg.Hermes.APIKeyEnv),
			Profile:     runtimeAgent.Profile,
			Enabled:     enabled,
		}
		if err := db.SaveCloudAgent(agent); err != nil {
			return err
		}
	}
	return nil
}

func buildAgentRuntimes(
	cfg *config.Config,
	cloudClient *cloud.Client,
	db *store.Store,
) ([]collector.AgentRuntime, map[string]*executor.Executor, error) {
	agents, err := db.ListCloudAgents()
	if err != nil {
		return nil, nil, err
	}
	runtimes := make([]collector.AgentRuntime, 0, len(agents))
	executors := make(map[string]*executor.Executor, len(agents))
	for _, agent := range agents {
		apiKey, err := hermesAPIKey(agent.APIKeyEnv)
		if err != nil {
			log.Printf("[paxd] Hermes API key for agent %s unavailable: %v", agent.AgentID, err)
		}
		hermesClient := hermes.NewClient(agent.APIEndpoint, apiKey)
		runtimes = append(runtimes, collector.AgentRuntime{
			Agent:        agent,
			HermesClient: hermesClient,
		})
		executors[agent.AgentID] = executor.New(hermesClient, cloudClient, db, agent.AgentID)
	}
	return runtimes, executors, nil
}

func hermesAPIKey(path string) (string, error) {
	if value := os.Getenv("HERMES_API_KEY"); value != "" {
		return value, nil
	}
	if path == "" {
		return "", nil
	}
	return config.ReadEnvKey(path)
}

// cmdInstallService generates a macOS launchd plist.
func cmdInstallService() {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(os.Stderr, "install-service only supported on macOS")
		os.Exit(1)
	}

	execPath, err := os.Executable()
	if err != nil {
		log.Fatalf("get executable path: %v", err)
	}

	home, _ := os.UserHomeDir()
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.toddzheng.paxd</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>run</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s/.pax/paxd.log</string>
    <key>StandardErrorPath</key>
    <string>%s/.pax/paxd.log</string>
</dict>
</plist>
`, execPath, home, home)

	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.toddzheng.paxd.plist")
	os.MkdirAll(filepath.Dir(plistPath), 0755)

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		log.Fatalf("write plist: %v", err)
	}

	fmt.Printf("LaunchAgent installed at: %s\n", plistPath)
	fmt.Println("\nTo start:")
	fmt.Printf("  launchctl load %s\n", plistPath)
	fmt.Println("\nTo stop:")
	fmt.Printf("  launchctl unload %s\n", plistPath)
}

// yamlMarshal is a quick inline YAML marshaler.
func yamlMarshal(cfg *config.Config) ([]byte, error) {
	var sb strings.Builder
	sb.WriteString("# paxd configuration\n")
	sb.WriteString(fmt.Sprintf("agent:\n  name: %s\n  machine_type: %s\n  hostname: \"%s\"\n", cfg.Agent.Name, cfg.Agent.MachineType, cfg.Agent.Hostname))
	sb.WriteString(fmt.Sprintf("cloud:\n  api_url: %s\n  node_id: \"%s\"\n  api_key: \"%s\"\n", cfg.Cloud.APIURL, cfg.Cloud.NodeID, cfg.Cloud.APIKey))
	sb.WriteString(fmt.Sprintf("hermes:\n  api_endpoint: %s\n  api_key_from_env: %s\n  profile: %s\n", cfg.Hermes.APIEndpoint, cfg.Hermes.APIKeyEnv, cfg.Hermes.Profile))
	sb.WriteString(fmt.Sprintf("daemon:\n  poll_interval: %s\n  status_interval: %s\n  reconcile_interval: %s\n  log_level: %s\n  db_path: %s\n",
		cfg.Daemon.PollInterval, cfg.Daemon.StatusInterval, cfg.Daemon.ReconcileInterval, cfg.Daemon.LogLevel, cfg.Daemon.DBPath))
	return []byte(sb.String()), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
