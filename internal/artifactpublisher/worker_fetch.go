package artifactpublisher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pax-beehive/paxd/internal/artifactfetch"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

const codeLocalSnapshotUnavailable = "local_snapshot_unavailable"

// snapshotUserFetch copies a user_fetch source into the daemon-owned spool.
// The file is re-opened through the home root and the stat rules are re-run
// on the opened handle, because it may have changed since the command ack.
// Every failure is permanent: retrying cannot make a denied file fetchable.
func (w *Worker) snapshotUserFetch(ctx context.Context, job *daemonstore.ArtifactPublishJob) error {
	home, err := resolveHome(w.homeDir)
	if err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: err}
	}
	opened, denial, err := artifactfetch.Open(home, job.SourcePath)
	if err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: err}
	}
	if denial != nil {
		return permanentArtifactError{code: denial.Code, err: errors.New(denial.Message)}
	}
	defer opened.File.Close()

	finalDir := filepath.Dir(job.SpoolPath)
	if filepath.Base(finalDir) != job.PublicationID || filepath.Base(job.SpoolPath) != "content" {
		return permanentArtifactError{
			code: codeLocalSnapshotUnavailable,
			err:  fmt.Errorf("unexpected spool path for publication %s", job.PublicationID),
		}
	}
	rootDir := filepath.Dir(finalDir)
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: fmt.Errorf("create artifact spool root: %w", err)}
	}
	tempDir, err := os.MkdirTemp(rootDir, ".fetch-")
	if err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: fmt.Errorf("create artifact spool staging directory: %w", err)}
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	// Copy at most one byte past the limit; hashing then rejects a file that
	// grew beyond it, without letting a growing file fill the disk.
	limited := io.LimitReader(opened.File, maxSinglePutArtifactSizeBytes+1)
	if err := copySnapshot(filepath.Join(tempDir, "content"), limited); err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: err}
	}
	// A crash after a previous rename but before the status save leaves a
	// stale final directory; this fresh snapshot replaces it.
	if err := os.RemoveAll(finalDir); err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: fmt.Errorf("replace artifact snapshot: %w", err)}
	}
	if err := os.Rename(tempDir, finalDir); err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: fmt.Errorf("publish artifact snapshot: %w", err)}
	}
	job.SizeBytes = opened.Size
	job.Status = StatusAccepted
	return w.save(ctx, job)
}

// scanUserFetchSnapshot never includes matched text in the returned error.
func scanUserFetchSnapshot(job *daemonstore.ArtifactPublishJob) error {
	finding, err := artifactfetch.ScanFile(job.SpoolPath)
	if err != nil {
		return permanentArtifactError{code: codeLocalSnapshotUnavailable, err: fmt.Errorf("scan artifact snapshot: %w", err)}
	}
	if finding != nil {
		return permanentArtifactError{code: artifactfetch.CodeSecretDetected, err: errors.New(finding.Message())}
	}
	return nil
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func removeUserFetchSpool(job *daemonstore.ArtifactPublishJob) {
	dir := filepath.Dir(job.SpoolPath)
	if job.PublicationID == "" || filepath.Base(dir) != job.PublicationID {
		return
	}
	_ = os.RemoveAll(dir)
}
