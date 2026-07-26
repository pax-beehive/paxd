package artifactpublisher_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublisherGivenRegularFileWhenAcceptedThenSnapshotAndJobAreDurable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := filepath.Join(t.TempDir(), "report.txt")
	require.NoError(t, os.WriteFile(source, []byte("version one"), 0o600))
	store := openArtifactStore(t, filepath.Join(t.TempDir(), "paxd.db"))
	publisher := artifactpublisher.New(artifactpublisher.Options{
		Store:   store,
		RootDir: root,
		NewID:   func() (string, error) { return "apub_test_1", nil },
	})

	publication, err := publisher.Accept(ctx, artifactpublisher.Request{
		AgentID:    "agent_1",
		SessionID:  "session_1",
		SourcePath: source,
		Title:      "Report",
	})

	require.NoError(t, err)
	assert.Equal(t, "apub_test_1", publication.PublicationID)
	assert.Equal(t, artifactpublisher.StatusAccepted, publication.Status)
	job, err := store.GetArtifactPublishJob(ctx, publication.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, "agent_1", job.AgentID)
	assert.Equal(t, "session_1", job.SessionID)
	assert.Equal(t, "report.txt", job.SourceFilename)
	assert.Equal(t, "Report", job.DisplayTitle)
	assert.Equal(t, artifactpublisher.StatusAccepted, job.Status)
	require.FileExists(t, job.SpoolPath)

	require.NoError(t, os.WriteFile(source, []byte("version two"), 0o600))
	require.NoError(t, os.Remove(source))
	snapshot, err := os.ReadFile(job.SpoolPath)
	require.NoError(t, err)
	assert.Equal(t, "version one", string(snapshot))
}

func TestPublisherGivenPersistenceFailureWhenAcceptedThenItReturnsError(t *testing.T) {
	source := filepath.Join(t.TempDir(), "report.txt")
	require.NoError(t, os.WriteFile(source, []byte("content"), 0o600))
	publisher := artifactpublisher.New(artifactpublisher.Options{
		Store:   failingArtifactStore{err: errors.New("database unavailable")},
		RootDir: t.TempDir(),
		NewID:   func() (string, error) { return "apub_failed", nil },
	})

	publication, err := publisher.Accept(context.Background(), artifactpublisher.Request{
		AgentID:    "agent_1",
		SessionID:  "session_1",
		SourcePath: source,
	})

	require.Error(t, err)
	assert.Empty(t, publication.PublicationID)
}

func TestArtifactPublishJobGivenDaemonRestartWhenLoadedThenAcceptedJobRemains(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "paxd.db")
	store := openArtifactStore(t, dbPath)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	require.NoError(t, store.CreateArtifactPublishJob(ctx, daemonstore.ArtifactPublishJob{
		PublicationID:  "apub_restart",
		AgentID:        "agent_1",
		SessionID:      "session_1",
		SourceFilename: "report.txt",
		SpoolPath:      "/var/lib/paxd/artifact-spool/apub_restart/content",
		Status:         artifactpublisher.StatusAccepted,
		CreatedAt:      now,
		UpdatedAt:      now,
	}))
	closeArtifactStore(t, store)

	reopened := openArtifactStore(t, dbPath)
	jobs, err := reopened.ListUnfinishedArtifactPublishJobs(ctx)

	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "apub_restart", jobs[0].PublicationID)
	assert.Equal(t, artifactpublisher.StatusAccepted, jobs[0].Status)
}

func openArtifactStore(t *testing.T, path string) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	t.Cleanup(func() {
		sqlDB, err := store.DB().DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	return store
}

func closeArtifactStore(t *testing.T, store *daemonstore.Store) {
	t.Helper()
	sqlDB, err := store.DB().DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
}

type failingArtifactStore struct {
	err error
}

func (s failingArtifactStore) CreateArtifactPublishJob(
	context.Context,
	daemonstore.ArtifactPublishJob,
) error {
	return s.err
}
