package artifactpublisher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

const StatusAccepted = "accepted"

type Request = control.PublishArtifactRequest
type Publication = control.ArtifactPublication

type Store interface {
	CreateArtifactPublishJob(context.Context, daemonstore.ArtifactPublishJob) error
}

type Options struct {
	Store   Store
	RootDir string
	NewID   func() (string, error)
	Now     func() time.Time
}

type Service struct {
	store   Store
	rootDir string
	newID   func() (string, error)
	now     func() time.Time
}

func New(opts Options) *Service {
	newID := opts.NewID
	if newID == nil {
		newID = newPublicationID
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:   opts.Store,
		rootDir: opts.RootDir,
		newID:   newID,
		now:     now,
	}
}

func (s *Service) Accept(ctx context.Context, req Request) (Publication, error) {
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.SourcePath = strings.TrimSpace(req.SourcePath)
	req.Title = strings.TrimSpace(req.Title)
	if req.AgentID == "" {
		return Publication{}, errors.New("agent_id is required")
	}
	if req.SessionID == "" {
		return Publication{}, errors.New("session_id is required")
	}
	if req.SourcePath == "" {
		return Publication{}, errors.New("path is required")
	}
	if s.store == nil {
		return Publication{}, errors.New("artifact publish store is not configured")
	}
	if strings.TrimSpace(s.rootDir) == "" {
		return Publication{}, errors.New("artifact spool directory is not configured")
	}

	source, err := os.Open(req.SourcePath) // #nosec G304 -- the agent explicitly selects the local artifact.
	if err != nil {
		return Publication{}, fmt.Errorf("open artifact source: %w", err)
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return Publication{}, fmt.Errorf("stat artifact source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Publication{}, errors.New("artifact source must be a regular file")
	}
	filename := filepath.Base(req.SourcePath)
	if filename == "" || filename == "." || filename == ".." {
		return Publication{}, errors.New("artifact filename is invalid")
	}
	publicationID, err := s.newID()
	if err != nil {
		return Publication{}, fmt.Errorf("generate publication id: %w", err)
	}
	if err := os.MkdirAll(s.rootDir, 0o700); err != nil {
		return Publication{}, fmt.Errorf("create artifact spool root: %w", err)
	}
	tempDir, err := os.MkdirTemp(s.rootDir, ".accept-")
	if err != nil {
		return Publication{}, fmt.Errorf("create artifact spool staging directory: %w", err)
	}
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.RemoveAll(tempDir)
		}
	}()
	tempPath := filepath.Join(tempDir, "content")
	if err := copySnapshot(tempPath, source); err != nil {
		return Publication{}, err
	}
	finalDir := filepath.Join(s.rootDir, publicationID)
	if err := os.Rename(tempDir, finalDir); err != nil {
		return Publication{}, fmt.Errorf("publish artifact snapshot: %w", err)
	}
	keepTemp = true
	spoolPath := filepath.Join(finalDir, "content")
	now := s.now().UTC()
	job := daemonstore.ArtifactPublishJob{
		PublicationID:  publicationID,
		AgentID:        req.AgentID,
		SessionID:      req.SessionID,
		SourceFilename: filename,
		DisplayTitle:   req.Title,
		SpoolPath:      spoolPath,
		SizeBytes:      info.Size(),
		Status:         StatusAccepted,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.store.CreateArtifactPublishJob(ctx, job); err != nil {
		_ = os.RemoveAll(finalDir)
		return Publication{}, fmt.Errorf("persist artifact publish job: %w", err)
	}
	return Publication{
		PublicationID: publicationID,
		Status:        StatusAccepted,
		Filename:      filename,
	}, nil
}

func copySnapshot(path string, source *os.File) error {
	target, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create artifact snapshot: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = target.Close()
		}
	}()
	if _, err := io.Copy(target, source); err != nil {
		return fmt.Errorf("copy artifact snapshot: %w", err)
	}
	if err := target.Sync(); err != nil {
		return fmt.Errorf("sync artifact snapshot: %w", err)
	}
	if err := target.Close(); err != nil {
		return fmt.Errorf("close artifact snapshot: %w", err)
	}
	closed = true
	return nil
}

func newPublicationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "apub_" + hex.EncodeToString(raw[:]), nil
}
