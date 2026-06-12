// paxd is the Pax Fleet Daemon — runs on every agent machine,
// connects to the Fleet Cloud API, reports local Hermes session status,
// and executes messages from the Cloud mailbox.
//
// Usage:
//
//	paxd register --cloud-url https://fleet.example.com    # first-time registration
//	paxd run                                                  # start the daemon loop
//	paxd install-service                                      # install as macOS launchd service
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/toddzheng/paxd/internal/cloud"
	"github.com/toddzheng/paxd/internal/collector"
	"github.com/toddzheng/paxd/internal/config"
	"github.com/toddzheng/paxd/internal/executor"
	"github.com/toddzheng/paxd/internal/hermes"
	"github.com/toddzheng/paxd/internal/poller"
	"github.com/toddzheng/paxd/internal/state"
	"github.com/toddzheng/paxd/internal/store"
)

var version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
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
  paxd register --cloud-url <url>     first-time registration
  paxd run                             start the daemon loop
  paxd install-service                 install as macOS launchd service
  paxd --version                       print version
`, version)
}

// cmdRegister performs first-time registration with the Cloud API.
func cmdRegister(args []string) {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	cloudURL := fs.String("cloud-url", "", "Fleet Cloud API URL (required)")
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

	client := cloud.NewClient(*cloudURL, "")
	hostname, _ := os.Hostname()

	req := &cloud.RegisterRequest{
		Hostname:    hostname,
		MachineType: cfg.Agent.MachineType,
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
	}

	resp, err := client.Register(req)
	if err != nil {
		log.Fatalf("register: %v", err)
	}

	// Save registration result
	cfg.Cloud.APIURL = *cloudURL
	cfg.Cloud.APIKey = resp.APIKey

	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".pax")
	os.MkdirAll(configDir, 0700)
	configFile := filepath.Join(configDir, "paxd.yaml")

	data, err := yamlMarshal(cfg)
	if err != nil {
		log.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configFile, data, 0600); err != nil {
		log.Fatalf("write config: %v", err)
	}

	fmt.Printf("Registered successfully!\n")
	fmt.Printf("  agent_id: %s\n", resp.AgentID)
	fmt.Printf("  config:   %s\n", configFile)
	fmt.Printf("\nRun: paxd run\n")
}

// cmdRun starts the main daemon loop.
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
	agentState, err := db.GetAgentState()
	if err != nil {
		log.Fatalf("get agent state: %v", err)
	}

	// Transition to REGISTERING
	if err := sm.Transition(state.REGISTERING); err != nil {
		log.Fatalf("state transition: %v", err)
	}

	if agentState == nil {
		// Auto-register if config has API key
		if cfg.Cloud.APIKey == "" {
			log.Fatal("not registered. Run: paxd register --cloud-url <url>")
		}

		client := cloud.NewClient(cfg.Cloud.APIURL, cfg.Cloud.APIKey)
		hostname, _ := os.Hostname()
		req := &cloud.RegisterRequest{
			Hostname:    hostname,
			MachineType: cfg.Agent.MachineType,
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
		}
		resp, err := client.Register(req)
		if err != nil {
			log.Fatalf("register: %v", err)
		}

		agentState = &store.AgentState{
			AgentID:      resp.AgentID,
			CloudAPIKey:  cfg.Cloud.APIKey,
			CloudAPIURL:  cfg.Cloud.APIURL,
			RegisteredAt: time.Now().UTC().Format(time.RFC3339),
		}
		if err := db.SaveAgentState(agentState); err != nil {
			log.Fatalf("save agent state: %v", err)
		}
		log.Printf("[paxd] registered as agent %s", resp.AgentID)
	}

	// Resolve Hermes API key
	hermesAPIKey, err := cfg.Hermes.HermesAPIKey()
	if err != nil {
		log.Fatalf("hermes api key: %v", err)
	}

	// Create clients
	hermesClient := hermes.NewClient(cfg.Hermes.APIEndpoint, hermesAPIKey)
	cloudClient := cloud.NewClient(cfg.Cloud.APIURL, cfg.Cloud.APIKey)

	// Check Hermes reachability
	if err := hermesClient.Ping(); err != nil {
		log.Printf("[paxd] WARNING: Hermes not reachable: %v", err)
		log.Printf("[paxd] continuing — will retry on each status cycle")
	} else {
		log.Printf("[paxd] Hermes reachable at %s", cfg.Hermes.APIEndpoint)
	}

	// Create subsystems
	col := collector.New(hermesClient, cloudClient, db, agentState.AgentID)
	exec := executor.New(hermesClient, cloudClient, db, agentState.AgentID)
	pol := poller.New(cloudClient, exec, db)

	// Transition to RUNNING
	if err := sm.Transition(state.RUNNING); err != nil {
		log.Fatalf("state transition: %v", err)
	}
	log.Printf("[paxd] RUNNING (poll=%s, status=%s)", cfg.Daemon.PollInterval, cfg.Daemon.StatusInterval)

	// Main loop
	statusTicker := time.NewTicker(cfg.Daemon.StatusInterval)
	pollTicker := time.NewTicker(cfg.Daemon.PollInterval)
	defer statusTicker.Stop()
	defer pollTicker.Stop()

	// Graceful shutdown
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

		case <-statusTicker.C:
			if err := col.CollectAndReport(sm.Context()); err != nil {
				log.Printf("[paxd] status error: %v", err)
			}

		case <-pollTicker.C:
			if err := pol.PollAndExecute(sm.Context()); err != nil {
				log.Printf("[paxd] poll error: %v", err)
			}
		}
	}
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

// yamlMarshal is a quick inline YAML marshaler to avoid importing gopkg.in/yaml.v3 in main.
func yamlMarshal(cfg *config.Config) ([]byte, error) {
	var sb strings.Builder
	sb.WriteString("# paxd configuration\n")
	sb.WriteString(fmt.Sprintf("agent:\n  machine_type: %s\n  hostname: \"%s\"\n", cfg.Agent.MachineType, cfg.Agent.Hostname))
	sb.WriteString(fmt.Sprintf("cloud:\n  api_url: %s\n  api_key: \"%s\"\n", cfg.Cloud.APIURL, cfg.Cloud.APIKey))
	sb.WriteString(fmt.Sprintf("hermes:\n  api_endpoint: %s\n  api_key_from_env: %s\n  profile: %s\n", cfg.Hermes.APIEndpoint, cfg.Hermes.APIKeyEnv, cfg.Hermes.Profile))
	sb.WriteString(fmt.Sprintf("daemon:\n  poll_interval: %s\n  status_interval: %s\n  log_level: %s\n  db_path: %s\n",
		cfg.Daemon.PollInterval, cfg.Daemon.StatusInterval, cfg.Daemon.LogLevel, cfg.Daemon.DBPath))
	return []byte(sb.String()), nil
}
