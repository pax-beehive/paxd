// Package config loads and manages the paxd daemon configuration.
// Config is read from ~/.paxd/paxd.yaml on startup, with ~/.pax/paxd.yaml
// kept as a legacy fallback.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/machineinfo"
	"gopkg.in/yaml.v3"
)

const DefaultCloudAPIURL = "https://app.paxtech.net"

// Config represents the full daemon configuration.
type Config struct {
	AgentID      string               `yaml:"agent_id"`    // legacy/top-level shorthand
	InstanceID   string               `yaml:"instance_id"` // legacy/top-level shorthand
	Agent        AgentConfig          `yaml:"agent"`
	Cloud        CloudConfig          `yaml:"cloud"`
	Hermes       HermesConfig         `yaml:"hermes"`
	Agents       []RuntimeAgentConfig `yaml:"agents"`
	Daemon       DaemonConfig         `yaml:"daemon"`
	ACPForwarder ACPForwarderConfig   `yaml:"acp_forwarder"`
}

// AgentConfig identifies this machine.
type AgentConfig struct {
	AgentID     string `yaml:"agent_id"`
	Name        string `yaml:"name"`
	MachineType string `yaml:"machine_type"` // e.g. "mac_mini", "linux_box"
	Hostname    string `yaml:"hostname"`     // empty = auto-detect
}

// CloudConfig points to the Fleet Cloud API.
type CloudConfig struct {
	URL               string `yaml:"url"` // alias for api_url used by deployment docs
	APIURL            string `yaml:"api_url"`
	NodeID            string `yaml:"node_id"`
	APIKey            string `yaml:"api_key"`            // node key written by register command
	RegistrationToken string `yaml:"registration_token"` // one-time node registration token
	CFClientID        string `yaml:"cf_client_id"`       // Cloudflare Access Service Token (agent auth)
	CFClientSecret    string `yaml:"cf_client_secret"`   // Cloudflare Access Service Token secret
}

// HermesConfig points to the local Hermes API Server.
type HermesConfig struct {
	APIEndpoint     string `yaml:"api_endpoint"`       // e.g. http://localhost:8642
	APIKeyEnv       string `yaml:"api_key_from_env"`   // path to Hermes .env file
	APIKeySecretRef string `yaml:"api_key_secret_ref"` // cloud vault ref, e.g. sec_x@latest
	Profile         string `yaml:"profile"`            // Hermes profile name
}

// RuntimeAgentConfig describes one local agent hosted by this paxd node.
type RuntimeAgentConfig struct {
	AgentID         string                  `yaml:"agent_id"`
	InstanceID      string                  `yaml:"instance_id"`
	Name            string                  `yaml:"name"`
	AgentType       string                  `yaml:"agent_type"`
	APIEndpoint     string                  `yaml:"api_endpoint"`
	APIKeyEnv       string                  `yaml:"api_key_from_env"`
	APIKeySecretRef string                  `yaml:"api_key_secret_ref"`
	Profile         string                  `yaml:"profile"`
	Enabled         *bool                   `yaml:"enabled"`
	ACPForwarder    AgentACPForwarderConfig `yaml:"acp_forwarder"`
}

// DaemonConfig controls daemon behaviour.
type DaemonConfig struct {
	StatusInterval               time.Duration `yaml:"status_interval"`    // status report interval (default 10s)
	ReconcileInterval            time.Duration `yaml:"reconcile_interval"` // orphan reconciliation interval (default 15s)
	LogLevel                     string        `yaml:"log_level"`          // debug, info, warn, error
	LogFile                      string        `yaml:"log_file"`           // rolling log path (default ~/.paxd/logs/paxd.log, empty keeps stderr only)
	LogMaxSizeMB                 int           `yaml:"log_max_size_mb"`    // rotate threshold in MB (default 20)
	LogMaxBackups                int           `yaml:"log_max_backups"`    // rotated files to keep (default 3)
	DBPath                       string        `yaml:"db_path"`            // SQLite path (default ~/.paxd/paxd.db)
	PollInterval                 time.Duration `yaml:"poll_interval"`      // DEPRECATED: kept for config compat
	SessionBatchSize             int           `yaml:"session_batch_size"` // max sessions per status report
	TransportJournalGCInterval   time.Duration `yaml:"transport_journal_gc_interval"`
	TransportJournalKeepAckedFor time.Duration `yaml:"transport_journal_keep_acked_for"`
	TransportJournalKeepLatest   int           `yaml:"transport_journal_keep_latest"`
	TransportJournalGCBatchSize  int           `yaml:"transport_journal_gc_batch_size"`
}

// ACPForwarderConfig controls the stateless ACP tunnel forwarder.
type ACPForwarderConfig struct {
	Enabled           bool          `yaml:"enabled"`
	Harness           string        `yaml:"harness"`
	Command           []string      `yaml:"command"`
	WorkingDir        string        `yaml:"working_dir"`
	TunnelPath        string        `yaml:"tunnel_path"`
	ReconnectInterval time.Duration `yaml:"reconnect_interval"`
}

// AgentACPForwarderConfig overrides ACP forwarding for one hosted agent.
type AgentACPForwarderConfig struct {
	Enabled           *bool         `yaml:"enabled"`
	Harness           string        `yaml:"harness"`
	Command           []string      `yaml:"command"`
	WorkingDir        string        `yaml:"working_dir"`
	TunnelPath        string        `yaml:"tunnel_path"`
	ReconnectInterval time.Duration `yaml:"reconnect_interval"`
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Agent: AgentConfig{
			MachineType: machineinfo.Name(),
		},
		Cloud: CloudConfig{
			URL:    DefaultCloudAPIURL,
			APIURL: DefaultCloudAPIURL,
		},
		Hermes: HermesConfig{
			APIEndpoint: "http://localhost:8642",
		},
		Daemon: DaemonConfig{
			StatusInterval:               10 * time.Second,
			ReconcileInterval:            15 * time.Second,
			PollInterval:                 5 * time.Second,
			SessionBatchSize:             100,
			LogLevel:                     "info",
			LogFile:                      filepath.Join(home, ".paxd", "logs", "paxd.log"),
			LogMaxSizeMB:                 20,
			LogMaxBackups:                3,
			DBPath:                       filepath.Join(home, ".paxd", "paxd.db"),
			TransportJournalGCInterval:   10 * time.Minute,
			TransportJournalKeepAckedFor: 72 * time.Hour,
			TransportJournalKeepLatest:   100,
			TransportJournalGCBatchSize:  1000,
		},
		ACPForwarder: ACPForwarderConfig{
			TunnelPath:        "/api/v1/agent/tunnel",
			ReconnectInterval: 2 * time.Second,
		},
	}
}

// DefaultPath returns the preferred config path for new installations.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".paxd", "paxd.yaml")
}

// LegacyPath returns the previous config path, kept for compatibility.
func LegacyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pax", "paxd.yaml")
}

// Load reads the config file from the default path (~/.paxd/paxd.yaml)
// and merges it with defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path == "" {
		path = DefaultPath()
		if _, err := os.Stat(path); os.IsNotExist(err) {
			legacy := LegacyPath()
			if _, legacyErr := os.Stat(legacy); legacyErr == nil {
				path = legacy
			}
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnv(&cfg)
			normalizeAliases(&cfg)
			cfg.Agent.MachineType = machineinfo.Resolve(cfg.Agent.MachineType)
			if cfg.Agent.Hostname == "" {
				host, _ := os.Hostname()
				cfg.Agent.Hostname = host
			}
			cfg.Daemon.DBPath = expandHome(cfg.Daemon.DBPath)
			cfg.Hermes.APIKeyEnv = expandHome(cfg.Hermes.APIKeyEnv)
			cfg.ACPForwarder.WorkingDir = expandHome(cfg.ACPForwarder.WorkingDir)
			return &cfg, nil // use defaults if no config file
		}
		return nil, fmt.Errorf("read config: %w", err)
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyEnv(&cfg)
	normalizeAliases(&cfg)
	cfg.Agent.MachineType = machineinfo.Resolve(cfg.Agent.MachineType)

	// Auto-detect hostname
	if cfg.Agent.Hostname == "" {
		host, _ := os.Hostname()
		cfg.Agent.Hostname = host
	}

	// Resolve ~ in db_path
	cfg.Daemon.DBPath = expandHome(cfg.Daemon.DBPath)
	cfg.Hermes.APIKeyEnv = expandHome(cfg.Hermes.APIKeyEnv)
	cfg.ACPForwarder.WorkingDir = expandHome(cfg.ACPForwarder.WorkingDir)
	for i := range cfg.Agents {
		cfg.Agents[i].APIKeyEnv = expandHome(cfg.Agents[i].APIKeyEnv)
		cfg.Agents[i].ACPForwarder.WorkingDir = expandHome(cfg.Agents[i].ACPForwarder.WorkingDir)
	}

	return &cfg, nil
}

// HermesAPIKey reads the Hermes API key from the configured .env file.
// Looks for API_SERVER_KEY=... line.
func (c *HermesConfig) HermesAPIKey() (string, error) {
	return ReadEnvKey(c.APIKeyEnv)
}

// ReadEnvKey reads API_SERVER_KEY from an env file path.
func ReadEnvKey(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("hermes.api_key_from_env not set")
	}
	path = expandHome(path)
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

// RuntimeAgents returns explicit agents or a single legacy Hermes agent.
func (c *Config) RuntimeAgents() []RuntimeAgentConfig {
	if len(c.Agents) > 0 {
		return c.Agents
	}
	enabled := true
	return []RuntimeAgentConfig{{
		InstanceID:      firstNonEmpty(c.InstanceID, "default"),
		AgentID:         c.Agent.AgentID,
		Name:            firstNonEmpty(c.Agent.Name, "hermes"),
		AgentType:       "hermes",
		APIEndpoint:     c.Hermes.APIEndpoint,
		APIKeyEnv:       c.Hermes.APIKeyEnv,
		APIKeySecretRef: c.Hermes.APIKeySecretRef,
		Profile:         c.Hermes.Profile,
		Enabled:         &enabled,
	}}
}

func applyEnv(cfg *Config) {
	setStringFromEnv(&cfg.Cloud.APIURL, "PAX_CLOUD_URL")
	setStringFromEnv(&cfg.Cloud.NodeID, "PAX_NODE_ID")
	setStringFromEnv(&cfg.Cloud.APIKey, "PAX_NODE_API_KEY")
	setStringFromEnv(&cfg.Cloud.APIKey, "PAX_API_KEY")
	setStringFromEnv(&cfg.Cloud.RegistrationToken, "PAX_REGISTRATION_TOKEN")
	setStringFromEnv(&cfg.Cloud.CFClientID, "PAX_CLOUD_CF_CLIENT_ID")
	setStringFromEnv(&cfg.Cloud.CFClientSecret, "PAX_CLOUD_CF_CLIENT_SECRET")
	setStringFromEnv(&cfg.Agent.AgentID, "PAX_AGENT_ID")
	setStringFromEnv(&cfg.AgentID, "PAX_AGENT_ID")
	setStringFromEnv(&cfg.InstanceID, "PAX_INSTANCE_ID")
	setStringFromEnv(&cfg.Agent.Name, "PAX_NODE_NAME")
	setStringFromEnv(&cfg.Agent.MachineType, "PAX_MACHINE_TYPE")
	setStringFromEnv(&cfg.Agent.Hostname, "PAX_HOSTNAME")
	setStringFromEnv(&cfg.Hermes.APIEndpoint, "HERMES_API_ENDPOINT")
	setStringFromEnv(&cfg.Hermes.APIKeyEnv, "HERMES_API_KEY_FROM_ENV")
	setStringFromEnv(&cfg.Hermes.APIKeySecretRef, "HERMES_API_KEY_SECRET_REF")
	setStringFromEnv(&cfg.Hermes.Profile, "HERMES_PROFILE")
	setStringFromEnv(&cfg.Daemon.DBPath, "PAXD_DB_PATH")
	setIntFromEnv(&cfg.Daemon.SessionBatchSize, "PAX_SESSION_REPORT_BATCH_SIZE")
	setDurationFromEnv(&cfg.Daemon.TransportJournalGCInterval, "PAX_TRANSPORT_JOURNAL_GC_INTERVAL")
	setDurationFromEnv(&cfg.Daemon.TransportJournalKeepAckedFor, "PAX_TRANSPORT_JOURNAL_KEEP_ACKED_FOR")
	setIntFromEnv(&cfg.Daemon.TransportJournalKeepLatest, "PAX_TRANSPORT_JOURNAL_KEEP_LATEST")
	setIntFromEnv(&cfg.Daemon.TransportJournalGCBatchSize, "PAX_TRANSPORT_JOURNAL_GC_BATCH_SIZE")
	setBoolFromEnv(&cfg.ACPForwarder.Enabled, "PAX_ACP_FORWARD_ENABLED")
	setStringFromEnv(&cfg.ACPForwarder.Harness, "PAX_ACP_HARNESS")
	setStringSliceFromEnv(&cfg.ACPForwarder.Command, "PAX_ACP_COMMAND")
	setStringFromEnv(&cfg.ACPForwarder.WorkingDir, "PAX_ACP_WORKING_DIR")
	setStringFromEnv(&cfg.ACPForwarder.TunnelPath, "PAX_ACP_TUNNEL_PATH")
	setDurationFromEnv(&cfg.ACPForwarder.ReconnectInterval, "PAX_ACP_RECONNECT_INTERVAL")
}

func normalizeAliases(cfg *Config) {
	if cfg.Cloud.APIURL == "" ||
		(cfg.Cloud.APIURL == DefaultCloudAPIURL &&
			cfg.Cloud.URL != "" &&
			cfg.Cloud.URL != DefaultCloudAPIURL) {
		cfg.Cloud.APIURL = cfg.Cloud.URL
	}
	if cfg.Cloud.URL == "" || cfg.Cloud.URL != cfg.Cloud.APIURL {
		cfg.Cloud.URL = cfg.Cloud.APIURL
	}
	if cfg.Agent.AgentID == "" {
		cfg.Agent.AgentID = cfg.AgentID
	}
	if cfg.AgentID == "" {
		cfg.AgentID = cfg.Agent.AgentID
	}
}

func setStringFromEnv(target *string, key string) {
	if value := os.Getenv(key); value != "" {
		*target = value
	}
}

func setBoolFromEnv(target *bool, key string) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		*target = true
	case "0", "false", "no", "off":
		*target = false
	}
}

func setStringSliceFromEnv(target *[]string, key string) {
	if value := os.Getenv(key); value != "" {
		*target = strings.Fields(value)
	}
}

func setDurationFromEnv(target *time.Duration, key string) {
	if value := os.Getenv(key); value != "" {
		duration, err := time.ParseDuration(value)
		if err == nil {
			*target = duration
		}
	}
}

func setIntFromEnv(target *int, key string) {
	if value := os.Getenv(key); value != "" {
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			*target = parsed
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}
