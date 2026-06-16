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
//	paxd configure --cloud-url https://fleet.example.com   # register node + agents
//	paxd register --cloud-url https://fleet.example.com    # node-only registration
//	paxd run                                                  # start the daemon loop
//	paxd install-service                                      # install as macOS launchd service
//	paxd --version                                            # print version
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pax-beehive/paxd/internal/acpforwarder"
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
	case "configure":
		cmdConfigure(os.Args[2:])
	case "register":
		cmdRegister(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "acp-forward":
		cmdACPForward(os.Args[2:])
	case "postman":
		cmdPostman(os.Args[2:])
	case "harnesses":
		cmdHarnesses(os.Args[2:])
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
  paxd configure --cloud-url <url>    register node + agent config
  paxd register --cloud-url <url>     node-only registration
  paxd run                             start the daemon loop
  paxd acp-forward                     run only the ACP tunnel forwarder
  paxd postman                         print Postman WebSocket URL and smoke messages
  paxd harnesses                       inspect local ACP harness support
  paxd install-service                 install as macOS launchd service
  paxd --version                       print version
`, version)
}

type stringListFlag []string

func (f *stringListFlag) String() string {
	return strings.Join(*f, ",")
}

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type configureAgentSpec struct {
	Name       string
	Harness    string
	InstanceID string
}

// cmdConfigure registers this node and its hosted agents, then writes paxd.yaml.
func cmdConfigure(args []string) {
	fs := flag.NewFlagSet("configure", flag.ExitOnError)
	cloudURL := fs.String("cloud-url", "", "Fleet Cloud API URL")
	registrationToken := fs.String("registration-token", "", "Node registration token for first bootstrap")
	configPath := fs.String("config", "", "Config file path (default: ~/.paxd/paxd.yaml)")
	nodeName := fs.String("node-name", "", "Node display name")
	machineType := fs.String("machine-type", "", "Machine type label")
	harness := fs.String("harness", "codex", "Default ACP harness when --agent is omitted")
	cfClientID := fs.String("cf-client-id", "", "Cloudflare Access service token client ID")
	cfClientSecret := fs.String("cf-client-secret", "", "Cloudflare Access service token secret")
	var agentFlags stringListFlag
	fs.Var(&agentFlags, "agent", "Agent spec name:harness[:instance_id], repeatable")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	cfg.Cloud.APIURL = firstNonEmpty(*cloudURL, cfg.Cloud.APIURL)
	cfg.Cloud.URL = cfg.Cloud.APIURL
	if cfg.Cloud.APIURL == "" {
		fmt.Fprintln(os.Stderr, "--cloud-url, cloud.api_url, or PAX_CLOUD_URL is required")
		os.Exit(1)
	}

	if *nodeName != "" {
		cfg.Agent.Name = *nodeName
	}
	if *machineType != "" {
		cfg.Agent.MachineType = *machineType
	}
	if cfg.Agent.Hostname == "" {
		hostname, _ := os.Hostname()
		cfg.Agent.Hostname = hostname
	}
	if *cfClientID != "" {
		cfg.Cloud.CFClientID = *cfClientID
	}
	if *cfClientSecret != "" {
		cfg.Cloud.CFClientSecret = *cfClientSecret
	}

	specs, err := parseConfigureAgentSpecs(agentFlags, *harness)
	if err != nil {
		log.Fatalf("parse agents: %v", err)
	}

	nodeAPIKey := cfg.Cloud.APIKey
	token := firstNonEmpty(*registrationToken, cfg.Cloud.RegistrationToken)
	if nodeAPIKey == "" && token == "" {
		token = promptLine("Registration token: ")
	}
	if nodeAPIKey == "" && token == "" {
		fmt.Fprintln(os.Stderr, "--registration-token, cloud.registration_token, or PAX_REGISTRATION_TOKEN is required")
		os.Exit(1)
	}
	if nodeAPIKey != "" && *registrationToken != "" {
		fmt.Fprintln(os.Stderr, "config already has cloud.api_key; omit --registration-token or remove the existing key")
		os.Exit(1)
	}

	cfg.Agents = cfg.Agents[:0]
	for i, spec := range specs {
		regToken := ""
		req := &cloud.RegisterNodeAgentRequest{
			Agent: cloud.RegisterNodeAgentPayload{
				Name:      spec.Name,
				AgentType: spec.Harness,
			},
		}
		if nodeAPIKey == "" {
			regToken = token
			req.Node = &cloud.RegisterNodeRequest{
				Name:        cfg.Agent.Name,
				Hostname:    cfg.Agent.Hostname,
				MachineType: cfg.Agent.MachineType,
				OS:          runtime.GOOS,
				Arch:        runtime.GOARCH,
				PaxdVersion: version,
				APIEndpoint: cfg.Hermes.APIEndpoint,
			}
		}

		resp, err := cloud.NewClient(cfg.Cloud.APIURL, nodeAPIKey).
			WithCloudflareAccess(cfg.Cloud.CFClientID, cfg.Cloud.CFClientSecret).
			RegisterNodeAgent(req, regToken)
		if err != nil {
			log.Fatalf("register agent %s: %v", spec.Name, err)
		}
		if resp.NodeID != "" {
			cfg.Cloud.NodeID = resp.NodeID
		}
		if resp.APIKey != "" {
			nodeAPIKey = resp.APIKey
			cfg.Cloud.APIKey = resp.APIKey
		}
		if resp.AgentID == "" {
			log.Fatalf("register agent %s: response missing agent_id", spec.Name)
		}
		enabled := true
		cfg.Agents = append(cfg.Agents, config.RuntimeAgentConfig{
			AgentID:     resp.AgentID,
			InstanceID:  firstNonEmpty(spec.InstanceID, spec.Name, resp.AgentID),
			Name:        spec.Name,
			AgentType:   spec.Harness,
			APIEndpoint: cfg.Hermes.APIEndpoint,
			APIKeyEnv:   cfg.Hermes.APIKeyEnv,
			Profile:     cfg.Hermes.Profile,
			Enabled:     &enabled,
		})
		if i == 0 {
			cfg.Agent.AgentID = resp.AgentID
			cfg.AgentID = resp.AgentID
			cfg.InstanceID = firstNonEmpty(spec.InstanceID, spec.Name, "default")
			cfg.ACPForwarder.Enabled = true
			cfg.ACPForwarder.Harness = spec.Harness
			cfg.ACPForwarder.Command = acpCommandForHarness(spec.Harness)
		}
		fmt.Printf("Registered agent: %s (%s) -> %s\n", spec.Name, spec.Harness, resp.AgentID)
	}
	cfg.Cloud.APIKey = nodeAPIKey
	cfg.Cloud.RegistrationToken = ""

	targetPath := *configPath
	if targetPath == "" {
		targetPath = config.DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0700); err != nil {
		log.Fatalf("create config dir: %v", err)
	}
	data, err := yamlMarshal(cfg)
	if err != nil {
		log.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(targetPath, data, 0600); err != nil {
		log.Fatalf("write config: %v", err)
	}

	fmt.Printf("Configured paxd successfully.\n")
	fmt.Printf("  node_id: %s\n", cfg.Cloud.NodeID)
	fmt.Printf("  config:  %s\n", targetPath)
	fmt.Printf("\nRun: paxd run\n")
}

// cmdRegister performs first-time registration with the Cloud API.
func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	cloudURL := fs.String("cloud-url", "", "Fleet Cloud API URL (required)")
	registrationToken := fs.String("registration-token", "", "Node registration token")
	configPath := fs.String("config", "", "Config file path (default: ~/.paxd/paxd.yaml)")
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

	client := cloud.NewClient(*cloudURL, "").
		WithCloudflareAccess(cfg.Cloud.CFClientID, cfg.Cloud.CFClientSecret)
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

	// Save registration result
	cfg.Cloud.APIURL = *cloudURL
	cfg.Cloud.NodeID = resp.NodeID
	cfg.Cloud.APIKey = resp.APIKey
	cfg.Cloud.RegistrationToken = ""

	configFile := *configPath
	if configFile == "" {
		configFile = config.DefaultPath()
	}
	os.MkdirAll(filepath.Dir(configFile), 0700)

	data, err := yamlMarshal(cfg)
	if err != nil {
		log.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		log.Fatalf("write config: %v", err)
	}

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
	configPath := fs.String("config", "", "Config file path (default: ~/.paxd/paxd.yaml)")
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
	cloudClient := cloud.NewClient(cloudURL, nodeState.CloudAPIKey).
		WithCloudflareAccess(cfg.Cloud.CFClientID, cfg.Cloud.CFClientSecret)
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
	if cfg.ACPForwarder.Enabled {
		forwardCfg := acpForwarderConfig(cfg, nodeState)
		go func() {
			if err := acpforwarder.New(forwardCfg).Run(sm.Context()); err != nil &&
				sm.Context().Err() == nil {
				log.Printf("[paxd] acp forwarder stopped: %v", err)
			}
		}()
		log.Printf("[paxd] ACP forwarder enabled (tunnel=%s, command=%q)",
			forwardCfg.TunnelPath,
			forwardCfg.Command)
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

// cmdACPForward runs only the stateless ACP tunnel forwarder.
func cmdACPForward(args []string) {
	fs := flag.NewFlagSet("acp-forward", flag.ExitOnError)
	configPath := fs.String("config", "", "Config file path (default: ~/.paxd/paxd.yaml)")
	cloudURL := fs.String("cloud-url", "", "Cloud API URL")
	apiKey := fs.String("api-key", "", "Node API key")
	agentID := fs.String("agent-id", "", "Cloud agent ID")
	instanceID := fs.String("instance-id", "", "Agent instance ID")
	cfClientID := fs.String("cf-client-id", "", "Cloudflare Access service token client ID")
	cfClientSecret := fs.String("cf-client-secret", "", "Cloudflare Access service token secret")
	harness := fs.String("harness", "", "ACP harness preset: hermes, codex, claude, claude-code, gemini, or custom")
	command := fs.String("command", "", "ACP command, for example: \"hermes acp\"")
	workingDir := fs.String("working-dir", "", "ACP command working directory")
	tunnelPath := fs.String("tunnel-path", "", "Agent tunnel path")
	reconnectInterval := fs.Duration("reconnect-interval", 0, "Reconnect interval")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	nodeState := &store.NodeState{
		NodeID:       cfg.Cloud.NodeID,
		CloudAPIKey:  cfg.Cloud.APIKey,
		CloudAPIURL:  cfg.Cloud.APIURL,
		RegisteredAt: time.Now().UTC().Format(time.RFC3339),
	}
	applyACPForwardOverrides(cfg, nodeState, acpForwardOverrides{
		cloudURL:          *cloudURL,
		apiKey:            *apiKey,
		agentID:           *agentID,
		instanceID:        *instanceID,
		cfClientID:        *cfClientID,
		cfClientSecret:    *cfClientSecret,
		harness:           *harness,
		command:           *command,
		workingDir:        *workingDir,
		tunnelPath:        *tunnelPath,
		reconnectInterval: *reconnectInterval,
	})
	if nodeState.CloudAPIKey == "" || nodeState.CloudAPIURL == "" {
		db, err := store.Open(cfg.Daemon.DBPath)
		if err != nil {
			log.Fatalf("open db: %v", err)
		}
		defer db.Close()
		saved, err := db.GetNodeState()
		if err != nil {
			log.Fatalf("get node state: %v", err)
		}
		if saved != nil {
			nodeState = saved
			applyACPForwardOverrides(cfg, nodeState, acpForwardOverrides{
				cloudURL:          *cloudURL,
				apiKey:            *apiKey,
				agentID:           *agentID,
				instanceID:        *instanceID,
				cfClientID:        *cfClientID,
				cfClientSecret:    *cfClientSecret,
				harness:           *harness,
				command:           *command,
				workingDir:        *workingDir,
				tunnelPath:        *tunnelPath,
				reconnectInterval: *reconnectInterval,
			})
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	forwardCfg := acpForwarderConfig(cfg, nodeState)
	log.Printf("[paxd] ACP forwarder starting (tunnel=%s, command=%q)",
		forwardCfg.TunnelPath,
		forwardCfg.Command)
	if err := acpforwarder.New(forwardCfg).Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("acp forwarder: %v", err)
	}
}

func cmdPostman(args []string) {
	fs := flag.NewFlagSet("postman", flag.ExitOnError)
	configPath := fs.String("config", "", "Config file path (default: ~/.paxd/paxd.yaml)")
	cloudURL := fs.String("cloud-url", "", "Cloud API URL")
	agentID := fs.String("agent-id", "", "Cloud agent ID")
	tunnelPath := fs.String("path", "/api/v1/user/self/agents/{agent_id}/tunnel", "User tunnel path")
	cwd := fs.String("cwd", "/tmp", "ACP session cwd for the smoke message")
	fs.Parse(args)

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	url, err := postmanTunnelURL(
		firstNonEmpty(*cloudURL, cfg.Cloud.APIURL),
		*tunnelPath,
		firstNonEmpty(*agentID, cfg.Agent.AgentID),
	)
	if err != nil {
		log.Fatalf("postman url: %v", err)
	}

	fmt.Printf("Postman WebSocket URL:\n%s\n\n", url)
	fmt.Println("Headers/cookies:")
	fmt.Println("Use the same Cloudflare user auth material that makes GET /api/v1/user/self/me work.")
	fmt.Println()
	fmt.Println("1. initialize")
	fmt.Println(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{},"clientInfo":{"name":"postman","version":"0.1.0"}}}`)
	fmt.Println()
	fmt.Println("2. authenticate (only if initialize returns authMethods)")
	fmt.Println(`{"jsonrpc":"2.0","id":2,"method":"authenticate","params":{"methodId":"deepseek"}}`)
	fmt.Println()
	fmt.Println("3. session/new")
	fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"session/new\",\"params\":{\"cwd\":%q,\"mcpServers\":[]}}\n", *cwd)
	fmt.Println()
	fmt.Println("4. session/prompt")
	fmt.Println(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session_id_here","prompt":[{"type":"text","text":"Say hello in one short sentence."}]}}`)
}

func cmdHarnesses(args []string) {
	fs := flag.NewFlagSet("harnesses", flag.ExitOnError)
	fs.Parse(args)

	fmt.Println("ACP harness support:")
	for _, status := range detectHarnesses() {
		command := "-"
		if len(status.Command) > 0 {
			command = strings.Join(status.Command, " ")
		}
		state := "unsupported"
		if status.Supported {
			state = "supported"
		} else if status.Installed {
			state = "installed"
		}
		fmt.Printf("  %-12s %-11s command=%s", status.Name, state, command)
		if status.Path != "" {
			fmt.Printf(" path=%s", status.Path)
		}
		if status.Note != "" {
			fmt.Printf(" note=%s", status.Note)
		}
		fmt.Println()
	}
}

type harnessStatus struct {
	Name      string
	Command   []string
	Path      string
	Installed bool
	Supported bool
	Note      string
}

func detectHarnesses() []harnessStatus {
	statuses := []harnessStatus{
		detectHarness("hermes", acpCommandForHarness("hermes"), []string{"acp"}),
		detectHarness("gemini", acpCommandForHarness("gemini"), []string{"--acp", "--experimental-acp"}),
		detectExternalAdapterHarness("claude-code", "claude-agent-acp", acpCommandForHarness("claude-code")),
		detectExternalAdapterHarness("codex", "codex-acp", acpCommandForHarness("codex")),
	}
	for i := range statuses {
		statuses[i].Note = harnessNote(statuses[i])
	}
	return statuses
}

func detectExternalAdapterHarness(name, adapterBinary string, command []string) harnessStatus {
	status := harnessStatus{Name: name, Command: acpCommandForHarness(name)}
	if len(command) > 0 {
		status.Command = command
	}
	if path, err := exec.LookPath(adapterBinary); err == nil {
		status.Path = path
		status.Installed = true
		status.Supported = true
		return status
	}
	if _, err := exec.LookPath("npx"); err == nil && len(status.Command) > 0 && status.Command[0] == "npx" {
		status.Installed = true
		status.Supported = true
	}
	return status
}

func detectHarness(name string, command []string, helpMarkers []string) harnessStatus {
	status := harnessStatus{Name: name, Command: command}
	binary := name
	if name == "claude-code" {
		binary = "claude"
	}
	if len(command) > 0 {
		binary = command[0]
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return status
	}
	status.Path = path
	status.Installed = true

	if len(command) == 0 {
		return status
	}

	output, err := exec.Command(binary, "--help").CombinedOutput()
	if err != nil && len(output) == 0 {
		return status
	}
	help := string(output)
	for _, marker := range helpMarkers {
		if strings.Contains(help, marker) {
			status.Supported = true
			return status
		}
	}
	return status
}

func harnessNote(status harnessStatus) string {
	if !status.Installed {
		return "binary not found"
	}
	if status.Supported {
		switch status.Name {
		case "gemini":
			if os.Getenv("GEMINI_API_KEY") == "" {
				return "ACP flag present; GEMINI_API_KEY may be required for stdio mode"
			}
		case "claude-code", "codex":
			if status.Path != "" {
				return "external ACP adapter binary detected"
			}
			return "using npx fallback; install adapter binary for offline/runtime stability"
		}
		return "native ACP entrypoint detected"
	}
	switch status.Name {
	case "claude-code":
		return "install claude-agent-acp or make npx available"
	case "codex":
		return "install codex-acp or make npx available"
	default:
		return "ACP entrypoint not detected"
	}
}

type acpForwardOverrides struct {
	cloudURL          string
	apiKey            string
	agentID           string
	instanceID        string
	cfClientID        string
	cfClientSecret    string
	harness           string
	command           string
	workingDir        string
	tunnelPath        string
	reconnectInterval time.Duration
}

func applyACPForwardOverrides(
	cfg *config.Config,
	nodeState *store.NodeState,
	overrides acpForwardOverrides,
) {
	if overrides.cloudURL != "" {
		cfg.Cloud.APIURL = overrides.cloudURL
		nodeState.CloudAPIURL = overrides.cloudURL
	}
	if overrides.apiKey != "" {
		cfg.Cloud.APIKey = overrides.apiKey
		nodeState.CloudAPIKey = overrides.apiKey
	}
	if overrides.agentID != "" {
		cfg.Agent.AgentID = overrides.agentID
		cfg.AgentID = overrides.agentID
	}
	if overrides.instanceID != "" {
		cfg.InstanceID = overrides.instanceID
	}
	if overrides.cfClientID != "" {
		cfg.Cloud.CFClientID = overrides.cfClientID
	}
	if overrides.cfClientSecret != "" {
		cfg.Cloud.CFClientSecret = overrides.cfClientSecret
	}
	if overrides.harness != "" {
		cfg.ACPForwarder.Harness = overrides.harness
	}
	if overrides.command != "" {
		cfg.ACPForwarder.Command = strings.Fields(overrides.command)
	}
	if overrides.workingDir != "" {
		cfg.ACPForwarder.WorkingDir = overrides.workingDir
	}
	if overrides.tunnelPath != "" {
		cfg.ACPForwarder.TunnelPath = overrides.tunnelPath
	}
	if overrides.reconnectInterval > 0 {
		cfg.ACPForwarder.ReconnectInterval = overrides.reconnectInterval
	}
}

func postmanTunnelURL(rawBase, tunnelPath, agentID string) (string, error) {
	if rawBase == "" {
		return "", fmt.Errorf("cloud url is required")
	}
	if agentID == "" {
		return "", fmt.Errorf("agent id is required")
	}

	base, err := url.Parse(strings.TrimRight(rawBase, "/"))
	if err != nil {
		return "", fmt.Errorf("parse cloud url: %w", err)
	}
	switch base.Scheme {
	case "https":
		base.Scheme = "wss"
	case "http":
		base.Scheme = "ws"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported cloud url scheme %q", base.Scheme)
	}
	if tunnelPath == "" {
		tunnelPath = "/api/v1/user/self/agents/{agent_id}/tunnel"
	}
	if !strings.HasPrefix(tunnelPath, "/") {
		tunnelPath = "/" + tunnelPath
	}
	path := strings.TrimRight(base.Path, "/") + strings.ReplaceAll(tunnelPath, "{agent_id}", agentID)
	rawPath := strings.TrimRight(base.EscapedPath(), "/") +
		strings.ReplaceAll(tunnelPath, "{agent_id}", url.PathEscape(agentID))
	base.Path = path
	base.RawPath = rawPath
	return base.String(), nil
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
		log.Fatal("not registered. Run paxd configure or set cloud.registration_token")
	}
	client := cloud.NewClient(cfg.Cloud.APIURL, "").
		WithCloudflareAccess(cfg.Cloud.CFClientID, cfg.Cloud.CFClientSecret)
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

func acpForwarderConfig(cfg *config.Config, nodeState *store.NodeState) acpforwarder.Config {
	agent := firstEnabledRuntimeAgent(cfg)
	command := cfg.ACPForwarder.Command
	if len(command) == 0 {
		command = acpCommandForHarness(firstNonEmpty(cfg.ACPForwarder.Harness, agent.AgentType))
	}
	return acpforwarder.Config{
		CloudURL:          firstNonEmpty(nodeState.CloudAPIURL, cfg.Cloud.APIURL),
		APIKey:            firstNonEmpty(nodeState.CloudAPIKey, cfg.Cloud.APIKey),
		CFClientID:        cfg.Cloud.CFClientID,
		CFClientSecret:    cfg.Cloud.CFClientSecret,
		AgentID:           firstNonEmpty(agent.AgentID, cfg.Agent.AgentID),
		InstanceID:        agent.InstanceID,
		Command:           command,
		WorkingDir:        cfg.ACPForwarder.WorkingDir,
		TunnelPath:        cfg.ACPForwarder.TunnelPath,
		ReconnectInterval: cfg.ACPForwarder.ReconnectInterval,
	}
}

func acpCommandForHarness(harness string) []string {
	switch normalizeHarness(harness) {
	case "", "custom":
		return nil
	case "hermes":
		return []string{"hermes", "acp"}
	case "codex":
		return externalACPAdapterCommand("codex-acp", "@zed-industries/codex-acp")
	case "claude", "claude-code":
		return externalACPAdapterCommand("claude-agent-acp", "@agentclientprotocol/claude-agent-acp")
	case "gemini":
		return []string{"gemini", "--acp"}
	case "acp":
		return nil
	default:
		return nil
	}
}

func externalACPAdapterCommand(binary, npmPackage string) []string {
	if _, err := exec.LookPath(binary); err == nil {
		return []string{binary}
	}
	return []string{"npx", "-y", npmPackage}
}

func normalizeHarness(harness string) string {
	harness = strings.ToLower(strings.TrimSpace(harness))
	harness = strings.ReplaceAll(harness, "_", "-")
	return harness
}

func normalizeConfigureHarness(harness string) string {
	harness = normalizeHarness(harness)
	if harness == "claude" {
		return "claude-code"
	}
	return harness
}

func parseConfigureAgentSpecs(values []string, defaultHarness string) ([]configureAgentSpec, error) {
	if len(values) == 0 {
		harness := normalizeConfigureHarness(defaultHarness)
		if harness == "" {
			harness = "codex"
		}
		return []configureAgentSpec{{
			Name:       harness,
			Harness:    harness,
			InstanceID: "default",
		}}, nil
	}

	specs := make([]configureAgentSpec, 0, len(values))
	for _, value := range values {
		parts := strings.Split(value, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("--agent %q must be name:harness[:instance_id]", value)
		}
		name := strings.TrimSpace(parts[0])
		harness := normalizeConfigureHarness(parts[1])
		instanceID := ""
		if len(parts) == 3 {
			instanceID = strings.TrimSpace(parts[2])
		}
		if name == "" {
			return nil, fmt.Errorf("--agent %q has empty name", value)
		}
		if harness == "" {
			return nil, fmt.Errorf("--agent %q has empty harness", value)
		}
		specs = append(specs, configureAgentSpec{
			Name:       name,
			Harness:    harness,
			InstanceID: firstNonEmpty(instanceID, name),
		})
	}
	return specs, nil
}

func promptLine(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	return strings.TrimSpace(text)
}

func firstEnabledRuntimeAgent(cfg *config.Config) config.RuntimeAgentConfig {
	for _, agent := range cfg.RuntimeAgents() {
		if agent.Enabled == nil || *agent.Enabled {
			return agent
		}
	}
	return config.RuntimeAgentConfig{}
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
	sb.WriteString(fmt.Sprintf("agent_id: %s\n", yamlQuote(cfg.AgentID)))
	sb.WriteString(fmt.Sprintf("instance_id: %s\n", yamlQuote(cfg.InstanceID)))
	sb.WriteString(fmt.Sprintf("agent:\n  agent_id: %s\n  name: %s\n  machine_type: %s\n  hostname: %s\n",
		yamlQuote(cfg.Agent.AgentID),
		yamlQuote(cfg.Agent.Name),
		yamlQuote(cfg.Agent.MachineType),
		yamlQuote(cfg.Agent.Hostname)))
	sb.WriteString(fmt.Sprintf("cloud:\n  api_url: %s\n  node_id: %s\n  api_key: %s\n",
		yamlQuote(cfg.Cloud.APIURL),
		yamlQuote(cfg.Cloud.NodeID),
		yamlQuote(cfg.Cloud.APIKey)))
	if cfg.Cloud.CFClientID != "" || cfg.Cloud.CFClientSecret != "" {
		sb.WriteString(fmt.Sprintf("  cf_client_id: %s\n  cf_client_secret: %s\n",
			yamlQuote(cfg.Cloud.CFClientID),
			yamlQuote(cfg.Cloud.CFClientSecret)))
	}
	sb.WriteString(fmt.Sprintf("hermes:\n  api_endpoint: %s\n  api_key_from_env: %s\n  profile: %s\n",
		yamlQuote(cfg.Hermes.APIEndpoint),
		yamlQuote(cfg.Hermes.APIKeyEnv),
		yamlQuote(cfg.Hermes.Profile)))
	if len(cfg.Agents) > 0 {
		sb.WriteString("agents:\n")
		for _, agent := range cfg.Agents {
			enabled := true
			if agent.Enabled != nil {
				enabled = *agent.Enabled
			}
			sb.WriteString(fmt.Sprintf("  - agent_id: %s\n    instance_id: %s\n    name: %s\n    agent_type: %s\n    api_endpoint: %s\n    api_key_from_env: %s\n    profile: %s\n    enabled: %t\n",
				yamlQuote(agent.AgentID),
				yamlQuote(agent.InstanceID),
				yamlQuote(agent.Name),
				yamlQuote(agent.AgentType),
				yamlQuote(agent.APIEndpoint),
				yamlQuote(agent.APIKeyEnv),
				yamlQuote(agent.Profile),
				enabled))
		}
	}
	sb.WriteString(fmt.Sprintf("daemon:\n  poll_interval: %s\n  status_interval: %s\n  reconcile_interval: %s\n  log_level: %s\n  db_path: %s\n",
		cfg.Daemon.PollInterval,
		cfg.Daemon.StatusInterval,
		cfg.Daemon.ReconcileInterval,
		yamlQuote(cfg.Daemon.LogLevel),
		yamlQuote(cfg.Daemon.DBPath)))
	sb.WriteString(fmt.Sprintf("acp_forwarder:\n  enabled: %t\n  harness: %s\n  command: %s\n  working_dir: %s\n  tunnel_path: %s\n  reconnect_interval: %s\n",
		cfg.ACPForwarder.Enabled,
		yamlQuote(cfg.ACPForwarder.Harness),
		yamlStringList(cfg.ACPForwarder.Command),
		yamlQuote(cfg.ACPForwarder.WorkingDir),
		yamlQuote(cfg.ACPForwarder.TunnelPath),
		cfg.ACPForwarder.ReconnectInterval))
	return []byte(sb.String()), nil
}

func yamlQuote(value string) string {
	return fmt.Sprintf("%q", value)
}

func yamlStringList(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
