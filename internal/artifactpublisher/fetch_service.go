package artifactpublisher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/artifactfetch"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

const (
	// OriginUserFetch marks jobs created by a console user fetching a local
	// file. Agent publish_artifact jobs keep the empty origin.
	OriginUserFetch = "user_fetch"

	StatusSnapshotting = "snapshotting"
)

type FetchStore interface {
	CreateArtifactPublishJob(context.Context, daemonstore.ArtifactPublishJob) error
	GetArtifactPublishJob(context.Context, string) (daemonstore.ArtifactPublishJob, error)
}

type FetchOptions struct {
	Store   FetchStore
	Targets TargetResolver
	RootDir string
	// HomeDir confines fetches; empty means the daemon user's home.
	HomeDir    string
	Now        func() time.Time
	OnAccepted func()
}

// FetchService implements control.ArtifactFetchService. It only validates,
// binds and persists; the worker performs the snapshot after the ack so a
// large file never blocks the single in-flight node-control request.
type FetchService struct {
	store      FetchStore
	targets    TargetResolver
	rootDir    string
	homeDir    string
	now        func() time.Time
	onAccepted func()
}

var _ control.ArtifactFetchService = (*FetchService)(nil)

func NewFetchService(opts FetchOptions) *FetchService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &FetchService{
		store:      opts.Store,
		targets:    opts.Targets,
		rootDir:    opts.RootDir,
		homeDir:    opts.HomeDir,
		now:        now,
		onAccepted: opts.OnAccepted,
	}
}

func (s *FetchService) Check(
	_ context.Context,
	_ control.Source,
	query control.CheckArtifactFetchQuery,
) (control.ArtifactFetchCheckResult, error) {
	home, err := resolveHome(s.homeDir)
	if err != nil {
		return control.ArtifactFetchCheckResult{}, err
	}
	result, err := artifactfetch.Check(home, query.Path)
	if err != nil {
		return control.ArtifactFetchCheckResult{}, err
	}
	out := control.ArtifactFetchCheckResult{
		Allowed:  result.Allowed,
		Path:     result.Path,
		Filename: result.Filename,
	}
	if result.Allowed {
		size := result.SizeBytes
		out.SizeBytes = &size
	}
	if result.Denial != nil {
		out.Denial = &control.ArtifactFetchDenial{
			Code: result.Denial.Code, Rule: result.Denial.Rule, Message: result.Denial.Message,
		}
	}
	return out, nil
}

func (s *FetchService) Fetch(
	ctx context.Context,
	src control.Source,
	cmd control.FetchArtifactCommand,
) (control.ArtifactFetchResult, error) {
	remoteID := strings.TrimSpace(src.RemoteID)
	if src.Kind != control.SourceRemote || remoteID == "" {
		return control.ArtifactFetchResult{}, unauthorized("local file fetch requires an authenticated remote source")
	}
	if s.store == nil || s.targets == nil || strings.TrimSpace(s.rootDir) == "" {
		return control.ArtifactFetchResult{}, errors.New("artifact fetch is not configured")
	}
	agentID := strings.TrimSpace(cmd.AgentID)
	sessionID := strings.TrimSpace(cmd.SessionID)
	// Bind the agent to the requesting remote, so one remote cannot fetch
	// through an agent that belongs to another remote on this node.
	target, err := s.targets.Resolve(ctx, "", agentID)
	if err != nil || strings.TrimSpace(target.RemoteID) != remoteID {
		return control.ArtifactFetchResult{}, unauthorized("the agent is not bound to the requesting remote")
	}
	sourcePath := filepath.Clean(cmd.Path)

	existing, err := s.store.GetArtifactPublishJob(ctx, cmd.PublicationID)
	switch {
	case err == nil:
		return existingFetchResult(existing, remoteID, agentID, sessionID, sourcePath)
	case !errors.Is(err, daemonstore.ErrNotFound):
		return control.ArtifactFetchResult{}, fmt.Errorf("load artifact publish job: %w", err)
	}

	home, err := resolveHome(s.homeDir)
	if err != nil {
		return control.ArtifactFetchResult{}, err
	}
	checked, err := artifactfetch.Check(home, cmd.Path)
	if err != nil {
		return control.ArtifactFetchResult{}, err
	}
	if checked.Denial != nil {
		return control.ArtifactFetchResult{}, control.ControlError{
			Code: checked.Denial.Code, Message: checked.Denial.Message, Target: checked.Denial.Rule,
		}
	}
	now := s.now().UTC()
	job := daemonstore.ArtifactPublishJob{
		PublicationID:  cmd.PublicationID,
		RemoteID:       remoteID,
		AgentID:        agentID,
		SessionID:      sessionID,
		SourceFilename: checked.Filename,
		SourcePath:     checked.Path,
		Origin:         OriginUserFetch,
		SpoolPath:      filepath.Join(s.rootDir, cmd.PublicationID, "content"),
		SizeBytes:      checked.SizeBytes,
		Status:         StatusSnapshotting,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateArtifactPublishJob(ctx, job); err != nil {
		// A concurrent duplicate may have won the insert; answer idempotently.
		if raced, getErr := s.store.GetArtifactPublishJob(ctx, cmd.PublicationID); getErr == nil {
			return existingFetchResult(raced, remoteID, agentID, sessionID, sourcePath)
		}
		return control.ArtifactFetchResult{}, fmt.Errorf("persist artifact fetch job: %w", err)
	}
	if s.onAccepted != nil {
		s.onAccepted()
	}
	return fetchResult(job), nil
}

func existingFetchResult(
	job daemonstore.ArtifactPublishJob,
	remoteID, agentID, sessionID, sourcePath string,
) (control.ArtifactFetchResult, error) {
	if job.Origin != OriginUserFetch || job.RemoteID != remoteID || job.AgentID != agentID ||
		job.SessionID != sessionID || job.SourcePath != sourcePath {
		return control.ArtifactFetchResult{}, control.ControlError{
			Code:    control.ErrCodeConflict,
			Message: "publication id already exists with a different fetch request",
			Target:  "publication_id",
		}
	}
	return fetchResult(job), nil
}

func fetchResult(job daemonstore.ArtifactPublishJob) control.ArtifactFetchResult {
	return control.ArtifactFetchResult{
		PublicationID: job.PublicationID,
		Status:        job.Status,
		Filename:      job.SourceFilename,
		SizeBytes:     job.SizeBytes,
	}
}

func unauthorized(message string) control.ControlError {
	return control.ControlError{Code: "unauthorized", Message: message}
}

func resolveHome(configured string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return configured, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return home, nil
}
