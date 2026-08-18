package artifactpublisher_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

func TestWorkerGivenAcceptedSnapshotWhenRunThenPublishesArtifactInBackground(t *testing.T) {
	store, job := workerFixture(t, []byte("artifact bytes"))
	manager := &fakeArtifactManager{
		prepare: artifactpublisher.PrepareResult{
			Status:     artifactpublisher.PrepareStatusUploadRequired,
			ArtifactID: "art_1",
			Upload: &artifactpublisher.UploadTicket{
				UploadID:       "artup_1",
				URL:            "https://upload.example/start",
				Headers:        map[string]string{"x-goog-resumable": "start"},
				ChunkAlignment: 256 * 1024,
			},
		},
	}
	manager.onRegister = func(target artifactpublisher.ManagerTarget, registered daemonstore.ArtifactPublishJob) error {
		stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
		require.NoError(t, err)
		assert.Equal(t, "remote_1", stored.RemoteID)
		assert.Equal(t, stored.RemoteID, target.RemoteID)
		assert.Equal(t, stored.RemoteID, registered.RemoteID)
		return nil
	}
	uploader := &fakeArtifactUploader{}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store:    store,
		Targets:  staticArtifactTarget{},
		Manager:  manager,
		Uploader: uploader,
		Now:      fixedArtifactTime,
	})

	err := worker.RunOnce(context.Background())

	require.NoError(t, err)
	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
	assert.Equal(t, "remote_1", stored.RemoteID)
	assert.Equal(t, "art_1", stored.ArtifactID)
	assert.Equal(t, "artup_1", stored.UploadID)
	sum := sha256.Sum256([]byte("artifact bytes"))
	assert.Equal(t, hex.EncodeToString(sum[:]), stored.SHA256)
	assert.Equal(t, int64(len("artifact bytes")), stored.UploadedBytes)
	assert.Equal(t, []string{"register", "prepare", "complete"}, manager.calls)
	require.Len(t, uploader.jobs, 1)
	assert.Equal(t, stored.SHA256, uploader.jobs[0].SHA256)
}

func TestWorkerGivenExistingArtifactWhenPreparedThenSkipsGCSUpload(t *testing.T) {
	store, job := workerFixture(t, []byte("same bytes"))
	manager := &fakeArtifactManager{
		prepare: artifactpublisher.PrepareResult{
			Status:     artifactpublisher.PrepareStatusAvailable,
			ArtifactID: "art_existing",
		},
	}
	uploader := &fakeArtifactUploader{}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: staticArtifactTarget{}, Manager: manager, Uploader: uploader,
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
	assert.Equal(t, "art_existing", stored.ArtifactID)
	assert.Empty(t, uploader.jobs)
	assert.Equal(t, []string{"register", "prepare"}, manager.calls)
}

func TestWorkerGivenRestartedUploadingJobWhenRunThenResumesPersistedSession(t *testing.T) {
	store, job := workerFixture(t, []byte("resume bytes"))
	job.Status = artifactpublisher.StatusUploading
	job.SHA256 = artifactSHA256([]byte("resume bytes"))
	job.ContentType = "application/octet-stream"
	job.ArtifactID = "art_resume"
	job.UploadID = "artup_resume"
	job.ResumableURL = "https://upload.example/session"
	job.UploadedBytes = 4
	require.NoError(t, store.SaveArtifactPublishJob(context.Background(), job))
	uploader := &fakeArtifactUploader{}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store:   store,
		Targets: staticArtifactTarget{},
		Manager: &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
			Status:     artifactpublisher.PrepareStatusUploadRequired,
			ArtifactID: "art_resume",
			Upload:     &artifactpublisher.UploadTicket{UploadID: "artup_resume"},
		}},
		Uploader: uploader,
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	require.Len(t, uploader.jobs, 1)
	assert.Equal(t, "https://upload.example/session", uploader.jobs[0].ResumableURL)
	assert.Equal(t, int64(4), uploader.jobs[0].UploadedBytes)
	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
}

func TestWorkerGivenPersistedRemoteWhenRetriedThenItDoesNotResolveToAnotherRemote(t *testing.T) {
	store, job := workerFixture(t, []byte("remote-bound bytes"))
	job.RemoteID = "remote_home"
	require.NoError(t, store.SaveArtifactPublishJob(context.Background(), job))
	targets := &recordingArtifactTarget{target: artifactpublisher.ManagerTarget{
		RemoteID: "remote_home", BaseURL: "https://home.example",
	}}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: targets,
		Manager: &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
			Status: artifactpublisher.PrepareStatusAvailable, ArtifactID: "art_remote_bound",
		}},
		Uploader: &fakeArtifactUploader{},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	require.Equal(t, []artifactTargetLookup{{remoteID: "remote_home", agentID: "agent_1"}}, targets.lookups)
	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, "remote_home", stored.RemoteID)
}

func TestWorkerGivenResolverReturnsDifferentRemoteThenItDoesNotContactManager(t *testing.T) {
	store, job := workerFixture(t, []byte("remote-bound bytes"))
	job.RemoteID = "remote_home"
	require.NoError(t, store.SaveArtifactPublishJob(context.Background(), job))
	manager := &fakeArtifactManager{}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store,
		Targets: &recordingArtifactTarget{target: artifactpublisher.ManagerTarget{
			RemoteID: "remote_other", BaseURL: "https://other.example",
		}},
		Manager:  manager,
		Uploader: &fakeArtifactUploader{},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusRetryWait, stored.Status)
	assert.Equal(t, "remote_home", stored.RemoteID)
	assert.ErrorContains(t, errors.New(stored.ErrorMessage), "does not match persisted remote")
	assert.Empty(t, manager.calls)
}

func TestWorkerGivenTransientUploadFailureWhenRunThenPersistsRetryState(t *testing.T) {
	store, job := workerFixture(t, []byte("retry bytes"))
	uploader := &fakeArtifactUploader{err: errors.New("network reset")}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store:   store,
		Targets: staticArtifactTarget{},
		Manager: &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
			Status:     artifactpublisher.PrepareStatusUploadRequired,
			ArtifactID: "art_retry",
			Upload:     &artifactpublisher.UploadTicket{UploadID: "artup_retry"},
		}},
		Uploader: uploader,
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusRetryWait, stored.Status)
	assert.Equal(t, "background_retry", stored.ErrorCode)
	assert.Contains(t, stored.ErrorMessage, "network reset")
}

func TestWorkerGivenPresignedPutTransportFailureThenDoesNotPersistTicketSecret(t *testing.T) {
	store, job := workerFixture(t, []byte("retry bytes"))
	secretURL := "https://bucket.example/object?X-Amz-Credential=credential&X-Amz-Signature=top-secret"
	uploader := &artifactpublisher.PresignedPutUploader{Client: artifactHTTPDoerFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{
				Op: http.MethodPut, URL: secretURL, Err: context.DeadlineExceeded,
			}
		},
	)}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: staticArtifactTarget{},
		Manager: &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
			Status: artifactpublisher.PrepareStatusUploadRequired, ArtifactID: "art_retry",
			Upload: &artifactpublisher.UploadTicket{
				UploadID: "artup_retry", Protocol: artifactpublisher.UploadProtocolS3PresignedPut,
				URL: secretURL,
			},
		}},
		Uploaders: map[string]artifactpublisher.Uploader{
			artifactpublisher.UploadProtocolS3PresignedPut: uploader,
		},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusRetryWait, stored.Status)
	assert.NotContains(t, stored.ErrorMessage, secretURL)
	assert.NotContains(t, strings.ToLower(stored.ErrorMessage), "x-amz-signature")
	assert.NotContains(t, stored.ErrorMessage, "top-secret")
}

func TestWorkerGivenWriteOncePreconditionFailureThenCompletesExistingObject(t *testing.T) {
	store, job := workerFixture(t, []byte("already stored bytes"))
	uploader := &artifactpublisher.PresignedPutUploader{Client: artifactHTTPDoerFunc(
		func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusPreconditionFailed,
				Body:       http.NoBody,
			}, nil
		},
	)}
	manager := &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
		Status: artifactpublisher.PrepareStatusUploadRequired, ArtifactID: "art_existing",
		Upload: &artifactpublisher.UploadTicket{
			UploadID: "artup_existing", Protocol: artifactpublisher.UploadProtocolS3PresignedPut,
			URL: "https://objects.example.test/already-stored",
		},
	}}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: staticArtifactTarget{}, Manager: manager,
		Uploaders: map[string]artifactpublisher.Uploader{
			artifactpublisher.UploadProtocolS3PresignedPut: uploader,
		},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
	assert.Equal(t, job.SizeBytes, stored.UploadedBytes)
	assert.Equal(t, []string{"register", "prepare", "complete"}, manager.calls)
}

func TestWorkerGivenUploadProtocolWhenRunThenDispatchesToMatchingUploader(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol string
		selected string
	}{
		{name: "GCS resumable", protocol: artifactpublisher.UploadProtocolGCSResumable, selected: "gcs"},
		{name: "S3 presigned PUT", protocol: artifactpublisher.UploadProtocolS3PresignedPut, selected: "s3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, job := workerFixture(t, []byte("dispatch bytes"))
			gcs := &fakeArtifactUploader{}
			s3 := &fakeArtifactUploader{}
			worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
				Store:   store,
				Targets: staticArtifactTarget{},
				Manager: &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
					Status:     artifactpublisher.PrepareStatusUploadRequired,
					ArtifactID: "art_dispatch",
					Upload: &artifactpublisher.UploadTicket{
						UploadID: "artup_dispatch",
						Protocol: tc.protocol,
					},
				}},
				Uploaders: map[string]artifactpublisher.Uploader{
					artifactpublisher.UploadProtocolGCSResumable:   gcs,
					artifactpublisher.UploadProtocolS3PresignedPut: s3,
				},
			})

			require.NoError(t, worker.RunOnce(context.Background()))

			if tc.selected == "gcs" {
				require.Len(t, gcs.jobs, 1)
				assert.Empty(t, s3.jobs)
			} else {
				require.Len(t, s3.jobs, 1)
				assert.Empty(t, gcs.jobs)
			}
			stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
			require.NoError(t, err)
			assert.Equal(t, artifactpublisher.StatusAvailable, stored.Status)
		})
	}
}

func TestWorkerGivenUnknownUploadProtocolWhenRunThenRetainsSpoolForRetry(t *testing.T) {
	store, job := workerFixture(t, []byte("retain these bytes"))
	uploader := &fakeArtifactUploader{}
	manager := &fakeArtifactManager{prepare: artifactpublisher.PrepareResult{
		Status:     artifactpublisher.PrepareStatusUploadRequired,
		ArtifactID: "art_unknown",
		Upload: &artifactpublisher.UploadTicket{
			UploadID: "artup_unknown",
			Protocol: "future_storage_protocol",
		},
	}}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: staticArtifactTarget{}, Manager: manager,
		Uploaders: map[string]artifactpublisher.Uploader{
			artifactpublisher.UploadProtocolGCSResumable: uploader,
		},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusRetryWait, stored.Status)
	assert.Equal(t, "background_retry", stored.ErrorCode)
	assert.Contains(t, stored.ErrorMessage, "unsupported artifact upload protocol")
	assert.Contains(t, stored.ErrorMessage, "future_storage_protocol")
	assert.FileExists(t, job.SpoolPath)
	assert.Empty(t, uploader.jobs)
	assert.Equal(t, []string{"register", "prepare"}, manager.calls)
}

func TestWorkerGivenMissingOwnedSnapshotWhenRunThenMarksPermanentFailure(t *testing.T) {
	store, job := workerFixture(t, []byte("missing bytes"))
	require.NoError(t, os.Remove(job.SpoolPath))
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store:    store,
		Targets:  staticArtifactTarget{},
		Manager:  &fakeArtifactManager{},
		Uploader: &fakeArtifactUploader{},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusFailed, stored.Status)
	assert.Equal(t, "local_snapshot_unavailable", stored.ErrorCode)
}

func TestWorkerGivenSnapshotOverSinglePutLimitWhenRunThenMarksPermanentFailure(t *testing.T) {
	store, job := workerFixture(t, []byte("placeholder"))
	const overS3SinglePutLimit = int64(5*1024*1024*1024 + 1)
	require.NoError(t, os.Truncate(job.SpoolPath, overS3SinglePutLimit))
	manager := &fakeArtifactManager{}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store:    store,
		Targets:  staticArtifactTarget{},
		Manager:  manager,
		Uploader: &fakeArtifactUploader{},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusFailed, stored.Status)
	assert.Equal(t, "artifact_too_large", stored.ErrorCode)
	assert.Contains(t, stored.ErrorMessage, "5 GiB single PUT limit")
	assert.Equal(t, []string{"register", "failed"}, manager.calls)
}

func TestWorkerGivenOversizeSnapshotWhenFailureReportFailsThenDoesNotRetry(t *testing.T) {
	store, job := workerFixture(t, []byte("placeholder"))
	const overS3SinglePutLimit = int64(5*1024*1024*1024 + 1)
	require.NoError(t, os.Truncate(job.SpoolPath, overS3SinglePutLimit))
	manager := &fakeArtifactManager{failErr: errors.New("manager unavailable")}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store:    store,
		Targets:  staticArtifactTarget{},
		Manager:  manager,
		Uploader: &fakeArtifactUploader{},
	})

	require.NoError(t, worker.RunOnce(context.Background()))
	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusFailed, stored.Status)
	assert.Equal(t, "artifact_too_large", stored.ErrorCode)
	assert.Contains(t, stored.ErrorMessage, "5 GiB single PUT limit")
	assert.Contains(t, stored.ErrorMessage, "manager unavailable")
	assert.Equal(t, []string{"register", "failed"}, manager.calls)
}

func TestWorkerGivenChangedOwnedSnapshotWhenRunThenMarksHashMismatch(t *testing.T) {
	store, job := workerFixture(t, []byte("version-one"))
	job.SHA256 = artifactSHA256([]byte("version-one"))
	require.NoError(t, store.SaveArtifactPublishJob(context.Background(), job))
	require.NoError(t, os.WriteFile(job.SpoolPath, []byte("version-two"), 0o600))
	manager := &fakeArtifactManager{}
	worker := artifactpublisher.NewWorker(artifactpublisher.WorkerOptions{
		Store: store, Targets: staticArtifactTarget{}, Manager: manager,
		Uploader: &fakeArtifactUploader{},
	})

	require.NoError(t, worker.RunOnce(context.Background()))

	stored, err := store.GetArtifactPublishJob(context.Background(), job.PublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusFailed, stored.Status)
	assert.Equal(t, "local_snapshot_hash_mismatch", stored.ErrorCode)
	assert.Equal(t, []string{"register", "failed"}, manager.calls)
}

func workerFixture(
	t *testing.T,
	content []byte,
) (*daemonstore.Store, daemonstore.ArtifactPublishJob) {
	t.Helper()
	store := openArtifactStore(t, filepath.Join(t.TempDir(), "paxd.db"))
	spoolPath := filepath.Join(t.TempDir(), "content")
	require.NoError(t, os.WriteFile(spoolPath, content, 0o600))
	now := fixedArtifactTime()
	job := daemonstore.ArtifactPublishJob{
		PublicationID:  "apub_worker",
		AgentID:        "agent_1",
		SessionID:      "session_1",
		SourceFilename: "report.bin",
		DisplayTitle:   "Report",
		SpoolPath:      spoolPath,
		SizeBytes:      int64(len(content)),
		Status:         artifactpublisher.StatusAccepted,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, store.CreateArtifactPublishJob(context.Background(), job))
	return store, job
}

type staticArtifactTarget struct{}

func (staticArtifactTarget) Resolve(
	context.Context,
	string,
	string,
) (artifactpublisher.ManagerTarget, error) {
	return artifactpublisher.ManagerTarget{
		RemoteID: "remote_1",
		BaseURL:  "https://manager.example",
	}, nil
}

type artifactTargetLookup struct {
	remoteID string
	agentID  string
}

type recordingArtifactTarget struct {
	target  artifactpublisher.ManagerTarget
	lookups []artifactTargetLookup
}

func (r *recordingArtifactTarget) Resolve(
	_ context.Context,
	remoteID string,
	agentID string,
) (artifactpublisher.ManagerTarget, error) {
	r.lookups = append(r.lookups, artifactTargetLookup{remoteID: remoteID, agentID: agentID})
	return r.target, nil
}

type fakeArtifactManager struct {
	prepare    artifactpublisher.PrepareResult
	calls      []string
	onRegister func(artifactpublisher.ManagerTarget, daemonstore.ArtifactPublishJob) error
	failErr    error
}

func (m *fakeArtifactManager) Register(
	_ context.Context,
	target artifactpublisher.ManagerTarget,
	job daemonstore.ArtifactPublishJob,
) error {
	m.calls = append(m.calls, "register")
	if m.onRegister != nil {
		return m.onRegister(target, job)
	}
	return nil
}

func (m *fakeArtifactManager) Prepare(
	context.Context,
	artifactpublisher.ManagerTarget,
	daemonstore.ArtifactPublishJob,
) (artifactpublisher.PrepareResult, error) {
	m.calls = append(m.calls, "prepare")
	return m.prepare, nil
}

func (m *fakeArtifactManager) Fail(
	context.Context,
	artifactpublisher.ManagerTarget,
	daemonstore.ArtifactPublishJob,
	string,
	string,
) error {
	m.calls = append(m.calls, "failed")
	return m.failErr
}

func (m *fakeArtifactManager) Complete(
	context.Context,
	artifactpublisher.ManagerTarget,
	daemonstore.ArtifactPublishJob,
) error {
	m.calls = append(m.calls, "complete")
	return nil
}

type fakeArtifactUploader struct {
	jobs []daemonstore.ArtifactPublishJob
	err  error
}

func (u *fakeArtifactUploader) Upload(
	ctx context.Context,
	job daemonstore.ArtifactPublishJob,
	ticket artifactpublisher.UploadTicket,
	progress func(string, int64) error,
) error {
	u.jobs = append(u.jobs, job)
	if u.err != nil {
		return u.err
	}
	return progress("https://upload.example/session", job.SizeBytes)
}

func artifactSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func fixedArtifactTime() time.Time {
	return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
}
