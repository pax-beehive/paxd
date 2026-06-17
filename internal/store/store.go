// Package store manages the local SQLite database for the daemon.
// Core tables:
//   - node_state: daemon node identity + cloud credentials + message offset
//   - cloud_agents: cloud agent IDs hosted by this node
//   - orphaned_messages: messages skipped due to steer conflicts
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Store wraps the SQLite connection and provides domain queries.
type Store struct {
	db *sql.DB
}

// NodeState is the single-row table holding daemon identity.
type NodeState struct {
	NodeID       string
	CloudAPIKey  string
	CloudAPIURL  string
	RegisteredAt string
	LastOffset   int64
	UpdatedAt    string
}

// AgentState is the legacy single-row table holding daemon identity.
type AgentState struct {
	AgentID      string
	CloudAPIKey  string
	CloudAPIURL  string
	RegisteredAt string
	LastOffset   int64
	UpdatedAt    string
}

// HermesInstance represents a local Hermes API Server.
type HermesInstance struct {
	InstanceID  string
	Name        string
	APIEndpoint string
	APIKey      string
	Profile     string
	Enabled     bool
}

// CloudAgent maps a cloud agent under the local paxd node to a Hermes instance.
type CloudAgent struct {
	AgentID     string
	InstanceID  string
	Name        string
	AgentType   string
	APIEndpoint string
	APIKeyEnv   string
	Profile     string
	Enabled     bool
}

// OrphanedMessage is a chat message that was skipped because
// the session was running — awaiting backfill.
type OrphanedMessage struct {
	MessageID  string
	AgentID    string
	SessionID  string
	Content    string
	CreatedAt  string
	RetryCount int
}

// Open opens (or creates) the SQLite database at the given path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// Connection pool: SQLite works best single-writer.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS node_state (
		node_id       TEXT PRIMARY KEY,
		cloud_api_key TEXT NOT NULL,
		cloud_api_url TEXT NOT NULL,
		registered_at TEXT NOT NULL,
		last_offset   INTEGER DEFAULT 0,
		updated_at    TEXT DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS agent_state (
		agent_id       TEXT PRIMARY KEY,
		cloud_api_key  TEXT NOT NULL,
		cloud_api_url  TEXT NOT NULL,
		registered_at  TEXT NOT NULL,
		last_offset    INTEGER DEFAULT 0,
		updated_at     TEXT DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS hermes_instances (
		instance_id   TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		api_endpoint  TEXT NOT NULL,
		api_key       TEXT NOT NULL,
		profile       TEXT,
		enabled       INTEGER DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS cloud_agents (
		agent_id     TEXT PRIMARY KEY,
		instance_id  TEXT NOT NULL,
		name         TEXT NOT NULL,
		agent_type   TEXT NOT NULL DEFAULT 'hermes',
		api_endpoint TEXT NOT NULL,
		api_key_env  TEXT,
		profile      TEXT,
		enabled      INTEGER DEFAULT 1,
		updated_at   TEXT DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS orphaned_messages (
		message_id    TEXT PRIMARY KEY,
		agent_id      TEXT NOT NULL,
		session_id    TEXT,
		content       TEXT NOT NULL,
		created_at    TEXT NOT NULL,
		retry_count   INTEGER DEFAULT 0
	);
	`
	_, err := db.Exec(schema)
	return err
}

// --- node_state ---

// GetNodeState returns the current node state (single row).
func (s *Store) GetNodeState() (*NodeState, error) {
	row := s.db.QueryRow(`
		SELECT node_id, cloud_api_key, cloud_api_url, registered_at, last_offset, updated_at
		FROM node_state
		LIMIT 1
	`)
	var n NodeState
	err := row.Scan(
		&n.NodeID,
		&n.CloudAPIKey,
		&n.CloudAPIURL,
		&n.RegisteredAt,
		&n.LastOffset,
		&n.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// SaveNodeState upserts the node state.
func (s *Store) SaveNodeState(n *NodeState) error {
	_, err := s.db.Exec(`
		INSERT INTO node_state (node_id, cloud_api_key, cloud_api_url, registered_at, last_offset, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(node_id) DO UPDATE SET
			cloud_api_key = excluded.cloud_api_key,
			cloud_api_url = excluded.cloud_api_url,
			last_offset  = excluded.last_offset,
			updated_at   = datetime('now')
	`, n.NodeID, n.CloudAPIKey, n.CloudAPIURL, n.RegisteredAt, n.LastOffset)
	return err
}

// UpdateNodeOffset sets the node message consumption offset.
func (s *Store) UpdateNodeOffset(offset int64) error {
	_, err := s.db.Exec(`UPDATE node_state SET last_offset = ?, updated_at = datetime('now')`, offset)
	return err
}

// --- agent_state ---

// GetAgentState returns the current agent state (single row).
func (s *Store) GetAgentState() (*AgentState, error) {
	row := s.db.QueryRow(`SELECT agent_id, cloud_api_key, cloud_api_url, registered_at, last_offset, updated_at FROM agent_state`)
	var a AgentState
	err := row.Scan(&a.AgentID, &a.CloudAPIKey, &a.CloudAPIURL, &a.RegisteredAt, &a.LastOffset, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// SaveAgentState upserts the agent state.
func (s *Store) SaveAgentState(a *AgentState) error {
	_, err := s.db.Exec(`
		INSERT INTO agent_state (agent_id, cloud_api_key, cloud_api_url, registered_at, last_offset, updated_at)
		VALUES (?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(agent_id) DO UPDATE SET
			cloud_api_key = excluded.cloud_api_key,
			cloud_api_url = excluded.cloud_api_url,
			last_offset  = excluded.last_offset,
			updated_at   = datetime('now')
	`, a.AgentID, a.CloudAPIKey, a.CloudAPIURL, a.RegisteredAt, a.LastOffset)
	return err
}

// UpdateOffset sets the message consumption offset.
func (s *Store) UpdateOffset(offset int64) error {
	_, err := s.db.Exec(`UPDATE agent_state SET last_offset = ?, updated_at = datetime('now')`, offset)
	return err
}

// --- hermes_instances ---

// ListHermesInstances returns all enabled Hermes instances.
func (s *Store) ListHermesInstances() ([]HermesInstance, error) {
	rows, err := s.db.Query(`SELECT instance_id, name, api_endpoint, api_key, profile, enabled FROM hermes_instances WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var instances []HermesInstance
	for rows.Next() {
		var h HermesInstance
		if err := rows.Scan(&h.InstanceID, &h.Name, &h.APIEndpoint, &h.APIKey, &h.Profile, &h.Enabled); err != nil {
			return nil, err
		}
		instances = append(instances, h)
	}
	return instances, rows.Err()
}

// --- cloud_agents ---

// ListCloudAgents returns all enabled cloud agents hosted by this node.
func (s *Store) ListCloudAgents() ([]CloudAgent, error) {
	rows, err := s.db.Query(`
		SELECT agent_id, instance_id, name, agent_type, api_endpoint, COALESCE(api_key_env, ''),
			COALESCE(profile, ''), enabled
		FROM cloud_agents
		WHERE enabled = 1
		ORDER BY name, agent_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []CloudAgent
	for rows.Next() {
		var a CloudAgent
		if err := rows.Scan(
			&a.AgentID,
			&a.InstanceID,
			&a.Name,
			&a.AgentType,
			&a.APIEndpoint,
			&a.APIKeyEnv,
			&a.Profile,
			&a.Enabled,
		); err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// SaveCloudAgent inserts or updates a cloud agent mapping.
func (s *Store) SaveCloudAgent(a *CloudAgent) error {
	_, err := s.db.Exec(`
		INSERT INTO cloud_agents (
			agent_id, instance_id, name, agent_type, api_endpoint, api_key_env, profile, enabled, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(agent_id) DO UPDATE SET
			instance_id = excluded.instance_id,
			name = excluded.name,
			agent_type = excluded.agent_type,
			api_endpoint = excluded.api_endpoint,
			api_key_env = excluded.api_key_env,
			profile = excluded.profile,
			enabled = excluded.enabled,
			updated_at = datetime('now')
	`, a.AgentID, a.InstanceID, a.Name, a.AgentType, a.APIEndpoint, a.APIKeyEnv, a.Profile, a.Enabled)
	return err
}

// DisableCloudAgentsExcept disables enabled cloud agents absent from the current config.
func (s *Store) DisableCloudAgentsExcept(agentIDs []string) error {
	if len(agentIDs) == 0 {
		_, err := s.db.Exec(`
			UPDATE cloud_agents
			SET enabled = 0, updated_at = datetime('now')
			WHERE enabled = 1
		`)
		return err
	}

	placeholders := strings.TrimRight(strings.Repeat("?,", len(agentIDs)), ",")
	args := make([]any, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		args = append(args, agentID)
	}
	_, err := s.db.Exec(`
		UPDATE cloud_agents
		SET enabled = 0, updated_at = datetime('now')
		WHERE enabled = 1 AND agent_id NOT IN (`+placeholders+`)
	`, args...)
	return err
}

// SaveHermesInstance inserts or updates a Hermes instance record.
func (s *Store) SaveHermesInstance(h *HermesInstance) error {
	_, err := s.db.Exec(`
		INSERT INTO hermes_instances (instance_id, name, api_endpoint, api_key, profile, enabled)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_id) DO UPDATE SET
			name = excluded.name,
			api_endpoint = excluded.api_endpoint,
			api_key = excluded.api_key,
			profile = excluded.profile,
			enabled = excluded.enabled
	`, h.InstanceID, h.Name, h.APIEndpoint, h.APIKey, h.Profile, h.Enabled)
	return err
}

// --- orphaned_messages ---

// SaveOrphaned inserts a skipped message for later reconciliation.
func (s *Store) SaveOrphaned(msg *OrphanedMessage) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO orphaned_messages (message_id, agent_id, session_id, content, created_at, retry_count)
		VALUES (?, ?, ?, ?, ?, ?)
	`, msg.MessageID, msg.AgentID, msg.SessionID, msg.Content, msg.CreatedAt, msg.RetryCount)
	return err
}

// ListOrphaned returns orphaned messages ordered by creation time,
// filtered to those with retry_count < 10.
func (s *Store) ListOrphaned() ([]OrphanedMessage, error) {
	rows, err := s.db.Query(`
		SELECT message_id, agent_id, session_id, content, created_at, retry_count
		FROM orphaned_messages
		WHERE retry_count < 10
		ORDER BY created_at
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []OrphanedMessage
	for rows.Next() {
		var m OrphanedMessage
		if err := rows.Scan(&m.MessageID, &m.AgentID, &m.SessionID, &m.Content, &m.CreatedAt, &m.RetryCount); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// IncrementOrphanedRetry bumps retry_count by 1.
func (s *Store) IncrementOrphanedRetry(messageID string) error {
	_, err := s.db.Exec(`UPDATE orphaned_messages SET retry_count = retry_count + 1 WHERE message_id = ?`, messageID)
	return err
}

// DeleteOrphaned removes a reconciled orphaned message.
func (s *Store) DeleteOrphaned(messageID string) error {
	_, err := s.db.Exec(`DELETE FROM orphaned_messages WHERE message_id = ?`, messageID)
	return err
}

// nowUTC returns current time as UTC string.
func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
