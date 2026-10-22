package control

import (
	"context"
	"errors"
	"log"
	"path/filepath"
	"regexp"
	"strings"
)

// User-initiated local file fetch (see docs/artifact_fetch.md). The console
// asks the manager to check a path, then to fetch it; paxd snapshots the file
// and feeds it into the existing artifact upload pipeline. These handlers are
// remote-only and bypass the CommandRecord store: idempotency is keyed by the
// manager-generated publication ID on the durable artifact job itself.

const (
	artifactFetchTargetType = "artifact_publication"
	errCodeUnauthorized     = "unauthorized"
	maxArtifactFetchPath    = 8192
	maxArtifactFetchIDBytes = 256
)

var publicationIDPattern = regexp.MustCompile(`^apub_[0-9a-f]{32}$`)

type CheckArtifactFetchQuery struct {
	Path string `json:"path"`
}

type FetchArtifactCommand struct {
	PublicationID string `json:"publication_id"`
	AgentID       string `json:"agent_id"`
	SessionID     string `json:"session_id"`
	Path          string `json:"path"`
}

type ArtifactFetchDenial struct {
	Code    string `json:"code"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

// ArtifactFetchCheckResult reports policy and file-state problems as
// Allowed=false with a Denial; QueryResult.Error is reserved for malformed
// requests and internal failures. SizeBytes is set only when allowed.
type ArtifactFetchCheckResult struct {
	Allowed   bool                 `json:"allowed"`
	Path      string               `json:"path"`
	Filename  string               `json:"filename"`
	SizeBytes *int64               `json:"size_bytes,omitempty"`
	Denial    *ArtifactFetchDenial `json:"denial,omitempty"`
}

type ArtifactFetchResult struct {
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	Filename      string `json:"filename"`
	SizeBytes     int64  `json:"size_bytes"`
}

// ArtifactFetchService is implemented by the artifact publisher. Fetch returns
// a ControlError for denials, conflicts and authorization failures (rejected
// acks); any other error is treated as internal (failed ack).
type ArtifactFetchService interface {
	Check(ctx context.Context, src Source, query CheckArtifactFetchQuery) (ArtifactFetchCheckResult, error)
	Fetch(ctx context.Context, src Source, cmd FetchArtifactCommand) (ArtifactFetchResult, error)
}

func (q CheckArtifactFetchQuery) Validate() error {
	if strings.TrimSpace(q.Path) == "" || len(q.Path) > maxArtifactFetchPath {
		return invalid("check_artifact_fetch.path", "path is required and must be at most 8192 bytes")
	}
	return nil
}

func (cmd FetchArtifactCommand) Validate() error {
	if !publicationIDPattern.MatchString(cmd.PublicationID) {
		return invalid("fetch_artifact.publication_id", "publication id must be apub_ followed by 32 lowercase hex characters")
	}
	if id := strings.TrimSpace(cmd.AgentID); id == "" || len(cmd.AgentID) > maxArtifactFetchIDBytes {
		return invalid("fetch_artifact.agent_id", "agent id is required and must be at most 256 characters")
	}
	if id := strings.TrimSpace(cmd.SessionID); id == "" || len(cmd.SessionID) > maxArtifactFetchIDBytes {
		return invalid("fetch_artifact.session_id", "session id is required and must be at most 256 characters")
	}
	if cmd.Path == "" || len(cmd.Path) > maxArtifactFetchPath ||
		strings.ContainsAny(cmd.Path, "\x00\r\n") || !filepath.IsAbs(cmd.Path) {
		return invalid("fetch_artifact.path", "path must be an absolute path of at most 8192 bytes without control characters")
	}
	return nil
}

func isAuthenticatedRemote(src Source) bool {
	return src.Kind == SourceRemote && strings.TrimSpace(src.RemoteID) != ""
}

func (s *ControlService) handleArtifactFetchCheck(ctx context.Context, src Source, query CheckArtifactFetchQuery) QueryResult {
	result := QueryResult{Type: QueryArtifactFetchCheck}
	if !isAuthenticatedRemote(src) {
		result.Error = &ControlError{Code: errCodeUnauthorized, Message: "local file fetch requires an authenticated remote node-control source"}
		return result
	}
	if s.artifactFetch == nil {
		result.Error = &ControlError{Code: ErrCodeInternal, Message: "artifact fetch is not configured"}
		return result
	}
	check, err := s.artifactFetch.Check(ctx, src, query)
	if err != nil {
		log.Printf("[paxd] artifact fetch check failed: %v", err)
		result.Error = &ControlError{Code: ErrCodeInternal, Message: "artifact fetch check failed"}
		return result
	}
	result.ArtifactFetchCheck = &check
	return result
}

func (s *ControlService) handleArtifactFetch(ctx context.Context, src Source, cmd Command) (CommandAck, error) {
	fetch := cmd.FetchArtifact
	targetID := fetch.PublicationID
	if !isAuthenticatedRemote(src) {
		return rejectedAck(cmd.CommandID, artifactFetchTargetType, targetID, ControlError{
			Code: errCodeUnauthorized, Message: "local file fetch requires an authenticated remote node-control source",
		}), nil
	}
	if cmd.CommandID != fetch.PublicationID {
		return rejectedAck(cmd.CommandID, artifactFetchTargetType, targetID,
			invalid("command_id", "command id must equal fetch_artifact.publication_id")), nil
	}
	if s.artifactFetch == nil {
		return failedAck(cmd.CommandID, artifactFetchTargetType, targetID, ControlError{
			Code: ErrCodeInternal, Message: "artifact fetch is not configured",
		}), nil
	}
	result, err := s.artifactFetch.Fetch(ctx, src, *fetch)
	if err != nil {
		var typed ControlError
		if errors.As(err, &typed) && typed.Code != "" && typed.Code != ErrCodeInternal {
			return rejectedAck(cmd.CommandID, artifactFetchTargetType, targetID, typed), nil
		}
		log.Printf("[paxd] artifact fetch command=%s failed: %v", cmd.CommandID, err)
		return failedAck(cmd.CommandID, artifactFetchTargetType, targetID, ControlError{
			Code: ErrCodeInternal, Message: "artifact fetch failed",
		}), nil
	}
	return CommandAck{
		CommandID: cmd.CommandID, OK: true, Status: CommandStatusApplied,
		TargetType: artifactFetchTargetType, TargetID: targetID,
		Result: &CommandResult{ArtifactFetch: &result},
	}, nil
}
