package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/pax-beehive/paxd/pkg/model"
)

type Store struct {
	db *sql.DB
}

type Session struct {
	ID                 string
	Agent              string
	NativeID           string
	Title              string
	Status             string
	Preview            string
	ProjectID          string
	WorkspaceRootsJSON string
	LastActive         string
	UpdatedAt          string
	LastListedAt       string
	LastSyncedAt       string
	CurrentSyncVersion int64
	RawJSON            string
}

type Element struct {
	SessionID     string         `json:"sessionId"`
	SyncVersion   int64          `json:"syncVersion,omitempty"`
	Seq           int64          `json:"seq"`
	Type          string         `json:"type"`
	Role          string         `json:"role,omitempty"`
	Model         string         `json:"model,omitempty"`
	StartedAt     string         `json:"startedAt,omitempty"`
	CompletedAt   string         `json:"completedAt,omitempty"`
	DurationMS    int64          `json:"durationMs,omitempty"`
	UsageJSON     string         `json:"-"`
	ContentText   string         `json:"contentText,omitempty"`
	NormalizedRaw map[string]any `json:"normalized,omitempty"`
	RawJSON       string         `json:"raw,omitempty"`
}

func DefaultPath() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "pax-session", "pax-session.sqlite"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "pax-session", "pax-session.sqlite"), nil
}

func Open(path string) (*Store, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
	CREATE TABLE IF NOT EXISTS sessions (
		id TEXT PRIMARY KEY,
		agent TEXT NOT NULL,
		native_id TEXT NOT NULL,
		title TEXT,
		status TEXT,
		preview TEXT,
		project_id TEXT,
		workspace_roots_json TEXT,
		last_active TEXT,
		updated_at TEXT,
		last_listed_at TEXT NOT NULL,
		last_synced_at TEXT,
		current_sync_version INTEGER DEFAULT 0,
		raw_json TEXT,
		UNIQUE(agent, native_id)
	);
	CREATE INDEX IF NOT EXISTS idx_sessions_agent_updated ON sessions(agent, updated_at);

	CREATE TABLE IF NOT EXISTS sync_runs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT,
		agent TEXT NOT NULL,
		started_at TEXT NOT NULL,
		completed_at TEXT,
		status TEXT NOT NULL,
		error TEXT
	);

	CREATE TABLE IF NOT EXISTS session_elements (
		session_id TEXT NOT NULL,
		sync_version INTEGER NOT NULL,
		seq INTEGER NOT NULL,
		type TEXT NOT NULL,
		role TEXT,
		model TEXT,
		started_at TEXT,
		completed_at TEXT,
		duration_ms INTEGER DEFAULT 0,
		usage_json TEXT,
		content_text TEXT,
		normalized_json TEXT,
		raw_json TEXT,
		PRIMARY KEY(session_id, sync_version, seq)
	);
	CREATE INDEX IF NOT EXISTS idx_session_elements_current ON session_elements(session_id, sync_version, seq);
	`)
	return err
}

func (s *Store) UpsertSessions(ctx context.Context, agent string, infos []model.SessionInfo) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, info := range infos {
		nativeID := firstNonEmpty(info.NativeID, trimAgentPrefix(agent, info.SessionID))
		id := agent + ":" + nativeID
		roots, _ := json.Marshal(info.WorkspaceRoots)
		raw, _ := json.Marshal(info)
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (
				id, agent, native_id, title, status, preview, project_id, workspace_roots_json,
				last_active, updated_at, last_listed_at, raw_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				title = excluded.title,
				status = excluded.status,
				preview = excluded.preview,
				project_id = excluded.project_id,
				workspace_roots_json = excluded.workspace_roots_json,
				last_active = excluded.last_active,
				updated_at = excluded.updated_at,
				last_listed_at = excluded.last_listed_at,
				raw_json = excluded.raw_json
		`, id, agent, nativeID, info.Name, info.Status, info.Preview, info.ProjectID, string(roots),
			info.LastActive, firstNonEmpty(info.UpdatedAt, info.LastActive), now, string(raw))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListSessions(ctx context.Context, agents []string, limit int) ([]Session, error) {
	args := []any{}
	where := ""
	if len(agents) > 0 {
		placeholders := make([]string, 0, len(agents))
		for _, agent := range agents {
			placeholders = append(placeholders, "?")
			args = append(args, agent)
		}
		where = "WHERE agent IN (" + strings.Join(placeholders, ",") + ")"
	}
	query := `SELECT id, agent, native_id, COALESCE(title, ''), COALESCE(status, ''),
		COALESCE(preview, ''), COALESCE(project_id, ''), COALESCE(workspace_roots_json, '[]'),
		COALESCE(last_active, ''), COALESCE(updated_at, ''), last_listed_at,
		COALESCE(last_synced_at, ''), COALESCE(current_sync_version, 0), COALESCE(raw_json, '')
		FROM sessions ` + where + ` ORDER BY COALESCE(updated_at, last_active, last_listed_at) DESC, id`
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.ID, &session.Agent, &session.NativeID, &session.Title, &session.Status,
			&session.Preview, &session.ProjectID, &session.WorkspaceRootsJSON, &session.LastActive,
			&session.UpdatedAt, &session.LastListedAt, &session.LastSyncedAt,
			&session.CurrentSyncVersion, &session.RawJSON); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (s *Store) FindSession(ctx context.Context, id, agent string) (Session, error) {
	if agent != "" && !strings.Contains(id, ":") {
		id = agent + ":" + id
	}
	query := `SELECT id, agent, native_id, COALESCE(title, ''), COALESCE(status, ''),
		COALESCE(preview, ''), COALESCE(project_id, ''), COALESCE(workspace_roots_json, '[]'),
		COALESCE(last_active, ''), COALESCE(updated_at, ''), last_listed_at,
		COALESCE(last_synced_at, ''), COALESCE(current_sync_version, 0), COALESCE(raw_json, '')
		FROM sessions WHERE id = ?`
	args := []any{id}
	if !strings.Contains(id, ":") {
		query = strings.Replace(query, "WHERE id = ?", "WHERE native_id = ?", 1)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Session{}, err
	}
	defer rows.Close()
	var matches []Session
	for rows.Next() {
		var session Session
		if err := rows.Scan(&session.ID, &session.Agent, &session.NativeID, &session.Title, &session.Status,
			&session.Preview, &session.ProjectID, &session.WorkspaceRootsJSON, &session.LastActive,
			&session.UpdatedAt, &session.LastListedAt, &session.LastSyncedAt,
			&session.CurrentSyncVersion, &session.RawJSON); err != nil {
			return Session{}, err
		}
		matches = append(matches, session)
	}
	if err := rows.Err(); err != nil {
		return Session{}, err
	}
	if len(matches) == 0 {
		return Session{}, sql.ErrNoRows
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		return Session{}, fmt.Errorf("session id %q is ambiguous: %s", id, strings.Join(ids, ", "))
	}
	return matches[0], nil
}

func (s *Store) BeginSync(ctx context.Context, sessionID, agent string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `INSERT INTO sync_runs (session_id, agent, started_at, status) VALUES (?, ?, ?, ?)`,
		sessionID, agent, now, "running")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) CompleteSync(ctx context.Context, sessionID string, version int64, elements []Element) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, element := range elements {
		normalized, _ := json.Marshal(element.NormalizedRaw)
		if element.Type == "" {
			element.Type = "message"
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO session_elements (
				session_id, sync_version, seq, type, role, model, started_at, completed_at,
				duration_ms, usage_json, content_text, normalized_json, raw_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, sessionID, version, element.Seq, element.Type, element.Role, element.Model, element.StartedAt,
			element.CompletedAt, element.DurationMS, element.UsageJSON, element.ContentText, string(normalized),
			element.RawJSON); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET current_sync_version = ?, last_synced_at = ? WHERE id = ?`,
		version, now, sessionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_runs SET completed_at = ?, status = ? WHERE id = ?`,
		now, "ok", version); err != nil {
		return err
	}
	if err := pruneOldVersions(ctx, tx, sessionID, 2); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailSync(ctx context.Context, version int64, syncErr error) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `UPDATE sync_runs SET completed_at = ?, status = ?, error = ? WHERE id = ?`,
		now, "error", syncErr.Error(), version)
	return err
}

func (s *Store) Elements(ctx context.Context, session Session) ([]Element, error) {
	if session.CurrentSyncVersion == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, sync_version, seq, type, COALESCE(role, ''), COALESCE(model, ''),
			COALESCE(started_at, ''), COALESCE(completed_at, ''), COALESCE(duration_ms, 0),
			COALESCE(usage_json, ''), COALESCE(content_text, ''), COALESCE(normalized_json, ''),
			COALESCE(raw_json, '')
		FROM session_elements
		WHERE session_id = ? AND sync_version = ?
		ORDER BY seq
	`, session.ID, session.CurrentSyncVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var elements []Element
	for rows.Next() {
		var element Element
		var normalized string
		if err := rows.Scan(&element.SessionID, &element.SyncVersion, &element.Seq, &element.Type,
			&element.Role, &element.Model, &element.StartedAt, &element.CompletedAt, &element.DurationMS,
			&element.UsageJSON, &element.ContentText, &normalized, &element.RawJSON); err != nil {
			return nil, err
		}
		if normalized != "" {
			_ = json.Unmarshal([]byte(normalized), &element.NormalizedRaw)
		}
		elements = append(elements, element)
	}
	return elements, rows.Err()
}

func pruneOldVersions(ctx context.Context, tx *sql.Tx, sessionID string, keep int) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT sync_version FROM session_elements
		WHERE session_id = ?
		ORDER BY sync_version DESC
	`, sessionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return err
		}
		versions = append(versions, version)
	}
	if len(versions) <= keep {
		return rows.Err()
	}
	for _, version := range versions[keep:] {
		if _, err := tx.ExecContext(ctx, `DELETE FROM session_elements WHERE session_id = ? AND sync_version = ?`,
			sessionID, version); err != nil {
			return err
		}
	}
	return rows.Err()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func trimAgentPrefix(agent, id string) string {
	prefix := agent + ":"
	return strings.TrimPrefix(id, prefix)
}
