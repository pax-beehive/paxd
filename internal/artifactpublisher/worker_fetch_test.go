package artifactpublisher_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactfetch"
	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

// fakeAWSKey is assembled at runtime so no literal token lives in the source.
var fakeAWSKey = "AK" + "IA" + strings.Repeat("Q7", 8)

type fetchWorkerFixture struct {
	store    *daemonstore.Store
	home     string
	root     string
	job      daemonstore.ArtifactPublishJob
	manager  *fakeArtifactManager
	uploader *fakeArtifactUploader
	worker   *artifactpublisher.Worker
}

func newFetchWorkerFixture(t *testing.T, content string, mode os.FileMode) *fetchWorkerFixture {
	t.Helper()
	f := &fetchWorkerFixture{
		store: openArtifactStore(t, filepath.Join(t.TempDir(), "paxd.db")),
		home:  t.TempDir(),
		root:  filepath.Join(t.TempDir(), "spool"),
		manager: &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
			Status: artifactpublisher.PrepareStatusUploadRequired, ArtifactID: "art_fetch",
			Upload: &artifactpublisher.UploadTicket{UploadID: "artup_fetch", Protocol: artifactpublisher.UploadProtocolS3PresignedPut},
		}},
		uploader: &fakeArtifactUploader{},
	}
	source := filepath.Join(f.home, "proj", "notes.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
	require.NoError(t, os.WriteFile(source, []byte(content), 0o600))
	require.NoError(t, os.Chmod(source, mode))
	now := fixedArtifactTime()
	f.job = daemonstore.ArtifactPublishJob{
		PublicationID:  fetchPublicationID,
		RemoteID:       "remote_1",
		AgentID:        "agent_1",
		SessionID:      "session_1",
		SourceFilename: "notes.md",
		SourcePath:     source,
		Origin:         artifactpublisher.OriginUserFetch,
		SpoolPath:      filepath.Join(f.root, fetchPublicationID, "content"),
		SizeBytes:      int64(len(content)),
		Status:         artifactpublisher.StatusSnapshotting,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, f.store.CreateArtifactPublishJob(context.Background(), f.job))
	f.worker = artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store:     f.store,
		Targets:   staticArtifactTarget{},
		Manager:   f.manager,
		Uploaders: map[string]artifactpublisher.Uploader{artifactpublisher.UploadProtocolS3PresignedPut: f.uploader},
		HomeDir:   f.home,
		Now:       fixedArtifactTime,
	})
	return f
}

func (f *fetchWorkerFixture) stored(t *testing.T) daemonstore.ArtifactPublishJob {
	t.Helper()
	job, err := f.store.GetArtifactPublishJob(context.Background(), f.job.PublicationID)
	require.NoError(t, err)
	return job
}

func TestWorkerGivenSnapshottingFetchWhenRunThenSnapshotsScansAndUploads(t *testing.T) {
	f := newFetchWorkerFixture(t, "# clean notes\n", 0o644)

	require.NoError(t, f.worker.RunOnce(context.Background()))

	stored := f.stored(t)
	assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
	assert.Equal(t, artifactSHA256([]byte("# clean notes\n")), stored.SHA256)
	assert.Equal(t, []string{"register", "prepare", "complete"}, f.manager.calls)
	require.Len(t, f.uploader.jobs, 1)
	snapshot, err := os.ReadFile(stored.SpoolPath)
	require.NoError(t, err)
	assert.Equal(t, "# clean notes\n", string(snapshot))
	info, err := os.Stat(stored.SpoolPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	leftovers, err := filepath.Glob(filepath.Join(f.root, ".fetch-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers)
}

func TestWorkerGivenFetchSourceBecameOwnerOnlyThenFailsWithoutUpload(t *testing.T) {
	f := newFetchWorkerFixture(t, "notes", 0o644)
	require.NoError(t, os.Chmod(f.job.SourcePath, 0o600))

	require.NoError(t, f.worker.RunOnce(context.Background()))

	stored := f.stored(t)
	assert.Equal(t, artifactpublisher.StatusFailed, stored.Status)
	assert.Equal(t, artifactfetch.CodeOwnerOnlyDenied, stored.ErrorCode)
	// Register precedes Fail: the manager rejects Fail for an unknown row.
	assert.Equal(t, []string{"register", "failed"}, f.manager.calls)
	assert.Equal(t, artifactfetch.CodeOwnerOnlyDenied, f.manager.failCode)
	assert.Empty(t, f.uploader.jobs)
	assert.NoDirExists(t, filepath.Dir(f.job.SpoolPath))
}

func TestWorkerGivenFetchSourceDeletedThenFailsNotFound(t *testing.T) {
	f := newFetchWorkerFixture(t, "notes", 0o644)
	require.NoError(t, os.Remove(f.job.SourcePath))

	require.NoError(t, f.worker.RunOnce(context.Background()))

	stored := f.stored(t)
	assert.Equal(t, artifactpublisher.StatusFailed, stored.Status)
	assert.Equal(t, artifactfetch.CodeNotFound, stored.ErrorCode)
	assert.Equal(t, artifactfetch.CodeNotFound, f.manager.failCode)
}

func TestWorkerGivenFetchRegisterFailsTransientlyThenRetrySnapshotsAndUploads(t *testing.T) {
	f := newFetchWorkerFixture(t, "# notes\n", 0o644)
	registerAttempts := 0
	f.manager.onRegister = func(artifactpublisher.ManagerTarget, daemonstore.ArtifactPublishJob) error {
		registerAttempts++
		if registerAttempts == 1 {
			return errors.New("manager unavailable")
		}
		return nil
	}

	require.NoError(t, f.worker.RunOnce(context.Background()))
	assert.Equal(t, artifactpublisher.StatusRetryWait, f.stored(t).Status)
	assert.NoFileExists(t, f.job.SpoolPath)

	require.NoError(t, f.worker.RunOnce(context.Background()))

	stored := f.stored(t)
	assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
	snapshot, err := os.ReadFile(stored.SpoolPath)
	require.NoError(t, err)
	assert.Equal(t, "# notes\n", string(snapshot))
	require.Len(t, f.uploader.jobs, 1)
}

func TestWorkerGivenFetchedFileWithSecretThenFailsBeforePrepareAndRemovesSpool(t *testing.T) {
	content := "# deploy\n\nsteps\n\naws_access_key_id = " + fakeAWSKey + "\n"
	f := newFetchWorkerFixture(t, content, 0o644)

	require.NoError(t, f.worker.RunOnce(context.Background()))

	stored := f.stored(t)
	assert.Equal(t, artifactpublisher.StatusFailed, stored.Status)
	assert.Equal(t, artifactfetch.CodeSecretDetected, stored.ErrorCode)
	assert.Contains(t, stored.ErrorMessage, "possible secret detected (rule aws-access-key-id, line 5)")
	assert.NotContains(t, stored.ErrorMessage, fakeAWSKey)
	assert.NotContains(t, f.manager.failMsg, fakeAWSKey)
	assert.Equal(t, artifactfetch.CodeSecretDetected, f.manager.failCode)
	assert.Equal(t, []string{"register", "failed"}, f.manager.calls, "prepare must not run before the scan passes")
	assert.Empty(t, f.uploader.jobs)
	assert.NoDirExists(t, filepath.Dir(f.job.SpoolPath))
}

func TestWorkerGivenAgentJobWithSecretThenStillUploadsUnchanged(t *testing.T) {
	store, job := workerFixture(t, []byte("aws_access_key_id = "+fakeAWSKey+"\n"))
	manager := &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
		Status: artifactpublisher.PrepareStatusUploadRequired, ArtifactID: "art_agent",
		Upload: &artifactpublisher.UploadTicket{UploadID: "artup_agent"},
	}}
	uploader := &fakeArtifactUploader{}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: staticArtifactTarget{}, Manager: manager, Uploader: uploader,
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Empty(t, stored.Origin)
	assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
	assert.Equal(t, []string{"register", "prepare", "complete"}, manager.calls)
	require.Len(t, uploader.jobs, 1)
}

func TestWorkerGivenAgentJobPermanentFailureThenKeepsSpool(t *testing.T) {
	store, job := workerFixture(t, []byte("version-one"))
	job.SHA256 = artifactSHA256([]byte("version-two"))
	require.NoError(t, store.SaveArtifactPublishJob(context.Background(), job))
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: staticArtifactTarget{}, Manager: &fakeArtifactManager{},
		Uploader: &fakeArtifactUploader{},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	assert.FileExists(t, job.SpoolPath, "agent spool is retained for diagnosis")
}
