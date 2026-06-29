package agentregistry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/pax-beehive/paxd/pkg/model"
)

const defaultHermesLocalSessionLimit = 500

func hermesLocalAvailable(command []string) bool {
	path, err := hermesStateDBPath(command)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func ListHermesLocalSessions(ctx context.Context, command []string, limit int) ([]model.SessionInfo, error) {
	path, err := hermesStateDBPath(command)
	if err != nil {
		return nil, err
	}
	return listHermesLocalSessionsFromDB(ctx, path, limit)
}

func hermesStateDBPath(command []string) (string, error) {
	home, err := defaultHermesHome()
	if err != nil {
		return "", err
	}
	if profile := hermesProfileFromCommand(command); profile != "" {
		return filepath.Join(home, profile, "state.db"), nil
	}
	return filepath.Join(home, "state.db"), nil
}

func hermesProfileFromCommand(command []string) string {
	for i := 1; i < len(command); i++ {
		arg := strings.TrimSpace(command[i])
		switch {
		case arg == "-p" || arg == "--profile":
			if i+1 < len(command) {
				return cleanHermesProfile(command[i+1])
			}
		case strings.HasPrefix(arg, "--profile="):
			return cleanHermesProfile(strings.TrimPrefix(arg, "--profile="))
		case strings.HasPrefix(arg, "-p="):
			return cleanHermesProfile(strings.TrimPrefix(arg, "-p="))
		case strings.HasPrefix(arg, "-p") && len(arg) > len("-p"):
			return cleanHermesProfile(strings.TrimPrefix(arg, "-p"))
		}
	}
	return ""
}

func cleanHermesProfile(profile string) string {
	profile = strings.TrimSpace(profile)
	if profile == "" || profile == "default" {
		return ""
	}
	return profile
}

func defaultHermesHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hermes"), nil
}

func listHermesLocalSessionsFromDB(ctx context.Context, path string, limit int) ([]model.SessionInfo, error) {
	if limit <= 0 {
		limit = defaultHermesLocalSessionLimit
	}
	db, err := sql.Open("sqlite3", hermesSQLiteReadOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	defer db.Close()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("open hermes state db: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT
			s.id,
			COALESCE(s.title, '') AS title,
			COALESCE(s.cwd, '') AS cwd,
			COALESCE(s.source, '') AS source,
			s.started_at,
			s.ended_at,
			COALESCE(s.end_reason, '') AS end_reason,
			COALESCE(s.input_tokens, 0) AS input_tokens,
			COALESCE(s.output_tokens, 0) AS output_tokens,
			COALESCE(s.cache_read_tokens, 0) AS cache_read_tokens,
			COALESCE(s.cache_write_tokens, 0) AS cache_write_tokens,
			COALESCE(s.reasoning_tokens, 0) AS reasoning_tokens,
			COALESCE(
				(
					SELECT MAX(m2.timestamp)
					FROM messages m2
					WHERE m2.session_id = s.id
					  AND COALESCE(m2.active, 1) = 1
				),
				s.started_at
			) AS last_active,
			COALESCE(
				(
					SELECT SUBSTR(REPLACE(REPLACE(m.content, X'0A', ' '), X'0D', ' '), 1, 160)
					FROM messages m
					WHERE m.session_id = s.id
					  AND COALESCE(m.active, 1) = 1
					  AND m.role = 'user'
					  AND m.content IS NOT NULL
					  AND m.content <> ''
					ORDER BY m.timestamp, m.id
					LIMIT 1
				),
				''
			) AS preview
		FROM sessions s
		WHERE COALESCE(s.archived, 0) = 0
		ORDER BY last_active DESC, s.started_at DESC, s.id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("query hermes sessions: %w", err)
	}
	defer rows.Close()

	var sessions []model.SessionInfo
	for rows.Next() {
		session, err := scanHermesLocalSession(rows)
		if err != nil {
			return nil, err
		}
		if session.SessionID != "" {
			sessions = append(sessions, session)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt > sessions[j].UpdatedAt
	})
	return sessions, nil
}

func hermesSQLiteReadOnlyDSN(path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("_busy_timeout", "5000")
	u.RawQuery = q.Encode()
	return u.String()
}

type hermesSessionRow interface {
	Scan(dest ...any) error
}

func scanHermesLocalSession(row hermesSessionRow) (model.SessionInfo, error) {
	var (
		id, title, cwd, source, endReason, preview string
		startedAt, lastActive                      float64
		endedAt                                    sql.NullFloat64
		inputTokens, outputTokens                  int64
		cacheReadTokens, cacheWriteTokens          int64
		reasoningTokens                            int64
	)
	if err := row.Scan(
		&id,
		&title,
		&cwd,
		&source,
		&startedAt,
		&endedAt,
		&endReason,
		&inputTokens,
		&outputTokens,
		&cacheReadTokens,
		&cacheWriteTokens,
		&reasoningTokens,
		&lastActive,
		&preview,
	); err != nil {
		return model.SessionInfo{}, err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return model.SessionInfo{}, errors.New("hermes session has empty id")
	}
	updatedAt := unixSecondsToRFC3339(lastActive)
	if updatedAt == "" {
		updatedAt = unixSecondsToRFC3339(startedAt)
	}
	tokenUsage := inputTokens + outputTokens + cacheReadTokens + cacheWriteTokens + reasoningTokens
	return model.SessionInfo{
		SessionID:      CanonicalSessionID("hermes", id),
		AgentType:      "hermes",
		NativeID:       id,
		Name:           firstNonEmpty(title, preview, source, id),
		ProjectID:      cwd,
		LastActive:     updatedAt,
		Preview:        preview,
		WorkspaceRoots: nonEmptySlice(cwd),
		Source:         source,
		Status:         hermesLocalStatus(endedAt, endReason),
		TokenUsage:     tokenUsage,
		UpdatedAt:      updatedAt,
	}, nil
}

func hermesLocalStatus(endedAt sql.NullFloat64, endReason string) string {
	if endedAt.Valid || strings.TrimSpace(endReason) != "" {
		return "completed"
	}
	return "available"
}

func unixSecondsToRFC3339(value float64) string {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return ""
	}
	seconds, fraction := math.Modf(value)
	return time.Unix(int64(seconds), int64(fraction*1e9)).UTC().Format(time.RFC3339Nano)
}
