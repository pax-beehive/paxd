// Package config loads and manages the paxd daemon configuration.
// Config is read from ~/.pax/paxd.yaml on startup.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the full daemon configuration.
type Config struct {
	Agent  AgentConfig  `yaml:"agent"`
	Cloud  CloudConfig  `yaml:"cloud"`
	Hermes HermesConfig `yaml:"hermes"`
	Daemon DaemonConfig `yaml:"daemon"`
}

// AgentConfig identifies this machine.
type AgentConfig struct {
	MachineType string `yaml:"machine_type"` // e.g. "mac_mini", "linux_box"
	Hostname    string `yaml:"hostname"`     // empty = auto-detect
}

// CloudConfig points to the Fleet Cloud API.
type CloudConfig struct {
	APIURL string `yaml:"api_url"`
	APIKey string `yaml:"api_key"` // written by register command
}

// HermesConfig points to the local Hermes API Server.
type HermesConfig struct {
	APIEndpoint  string `yaml:"api_endpoint"`    // e.g. http://localhost:8642
	APIKeyEnv    string `yaml:"api_key_from_env"` // path to Hermes .env file
	Profile      string `yaml:"profile"`          // Hermes profile name
}

// DaemonConfig controls daemon behaviour.
type DaemonConfig struct {
	PollInterval   time.Duration `yaml:"poll_interval"`   // message poll interval (default 5s)
	StatusInterval time.Duration `yaml:"status_interval"`  // status report interval (default 10s)
	LogLevel       string        `yaml:"log_level"`        // debug, info, warn, error
	DBPath         string        `yaml:"db_path"`          // SQLite path (default ~/.pax/paxd.db)
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Agent: AgentConfig{
			MachineType: "unknown",
		},
		Hermes: HermesConfig{
			APIEndpoint: "http://localhost:8642",
		},
		Daemon: DaemonConfig{
			PollInterval:   5 * time.Second,
			StatusInterval: 10 * time.Second,
			LogLevel:       "info",
			DBPath:         filepath.Join(home, ".pax", "paxd.db"),
		},
	}
}

// Load reads the config file from the default path (~/.pax/paxd.yaml)
// and merges it with defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("home dir: %w", err)
		}
		path = filepath.Join(home, ".pax", "paxd.yaml")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &cfg, nil // use defaults if no config file
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Auto-detect hostname
	if cfg.Agent.Hostname == "" {
		host, _ := os.Hostname()
		cfg.Agent.Hostname = host
	}

	// Resolve ~ in db_path
	cfg.Daemon.DBPath = expandHome(cfg.Daemon.DBPath)

	return &cfg, nil
}

// HermesAPIKey reads the Hermes API key from the configured .env file.
// Looks for API_SERVER_KEY=... line.
func (c *HermesConfig) HermesAPIKey() (string, error) {
	if c.APIKeyEnv == "" {
		return "", fmt.Errorf("hermes.api_key_from_env not set")
	}
	path := expandHome(c.APIKeyEnv)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read .env: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "API_SERVER_KEY=") {
			return strings.TrimPrefix(line, "API_SERVER_KEY="), nil
		}
	}
	return "", fmt.Errorf("API_SERVER_KEY not found in %s", path)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}
