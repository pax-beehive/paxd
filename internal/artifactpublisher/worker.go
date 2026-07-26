package artifactpublisher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/daemonstore"
)

const (
	StatusRegistering = "registering"
	StatusHashing     = "hashing"
	StatusPreparing   = "preparing"
	StatusUploading   = "uploading"
	StatusCompleting  = "completing"
	StatusAvailable   = "available"
	StatusRetryWait   = "retry_wait"
	StatusFailed      = "failed"

	PrepareStatusUploadRequired = "upload_required"
	PrepareStatusAvailable      = "available"

	defaultWorkerInterval = 5 * time.Second
)

var ErrResumableSessionExpired = errors.New("artifact resumable upload session expired")

type WorkerStore interface {
	ListUnfinishedArtifactPublishJobs(context.Context) ([]daemonstore.ArtifactPublishJob, error)
	SaveArtifactPublishJob(context.Context, daemonstore.ArtifactPublishJob) error
}

type ManagerTarget struct {
	RemoteID string
	BaseURL  string
	Headers  http.Header
}

type TargetResolver interface {
	Resolve(context.Context, string) (ManagerTarget, error)
}

type UploadTicket struct {
	UploadID       string            `json:"upload_id"`
	Protocol       string            `json:"protocol"`
	Method         string            `json:"method"`
	URL            string            `json:"url"`
	Headers        map[string]string `json:"headers"`
	ChunkAlignment int64             `json:"chunk_alignment"`
	ExpiresAt      time.Time         `json:"expires_at"`
}

type PrepareResult struct {
	Status     string        `json:"status"`
	ArtifactID string        `json:"artifact_id"`
	Upload     *UploadTicket `json:"upload,omitempty"`
}

type ManagerClient interface {
	Register(context.Context, ManagerTarget, daemonstore.ArtifactPublishJob) error
	Prepare(
		context.Context,
		ManagerTarget,
		daemonstore.ArtifactPublishJob,
	) (PrepareResult, error)
	Complete(context.Context, ManagerTarget, daemonstore.ArtifactPublishJob) error
	Fail(
		context.Context,
		ManagerTarget,
		daemonstore.ArtifactPublishJob,
		string,
		string,
	) error
}

type Uploader interface {
	Upload(
		context.Context,
		daemonstore.ArtifactPublishJob,
		UploadTicket,
		func(string, int64) error,
	) error
}

type WorkerOptions struct {
	Store    WorkerStore
	Targets  TargetResolver
	Manager  ManagerClient
	Uploader Uploader
	Interval time.Duration
	Now      func() time.Time
}

type Worker struct {
	store    WorkerStore
	targets  TargetResolver
	manager  ManagerClient
	uploader Uploader
	interval time.Duration
	now      func() time.Time
	wake     chan struct{}
	runMu    sync.Mutex
}

func NewWorker(opts WorkerOptions) *Worker {
	interval := opts.Interval
	if interval <= 0 {
		interval = defaultWorkerInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Worker{
		store:    opts.Store,
		targets:  opts.Targets,
		manager:  opts.Manager,
		uploader: opts.Uploader,
		interval: interval,
		now:      now,
		wake:     make(chan struct{}, 1),
	}
}

func (w *Worker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	go func() {
		_ = w.RunOnce(ctx)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = w.RunOnce(ctx)
			case <-w.wake:
				_ = w.RunOnce(ctx)
			}
		}
	}()
}

func (w *Worker) Wake() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *Worker) RunOnce(ctx context.Context) error {
	if w == nil || w.store == nil || w.targets == nil || w.manager == nil || w.uploader == nil {
		return nil
	}
	w.runMu.Lock()
	defer w.runMu.Unlock()

	jobs, err := w.store.ListUnfinishedArtifactPublishJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.process(ctx, &job); err != nil {
			w.recordFailure(ctx, &job, err)
			if saveErr := w.save(ctx, &job); saveErr != nil {
				return errors.Join(err, saveErr)
			}
		}
	}
	return nil
}

func (w *Worker) process(ctx context.Context, job *daemonstore.ArtifactPublishJob) error {
	target, err := w.targets.Resolve(ctx, job.AgentID)
	if err != nil {
		return fmt.Errorf("resolve artifact manager target: %w", err)
	}
	job.Status = StatusRegistering
	if err := w.save(ctx, job); err != nil {
		return err
	}
	if err := w.manager.Register(ctx, target, *job); err != nil {
		return fmt.Errorf("register artifact publication: %w", err)
	}
	expectedSHA256 := job.SHA256
	job.Status = StatusHashing
	if err := w.save(ctx, job); err != nil {
		return err
	}
	if err := hashArtifactSnapshot(job); err != nil {
		return permanentArtifactError{
			code: "local_snapshot_unavailable",
			err:  err,
		}
	}
	if expectedSHA256 != "" && !strings.EqualFold(expectedSHA256, job.SHA256) {
		return permanentArtifactError{
			code: "local_snapshot_hash_mismatch",
			err:  errors.New("artifact snapshot hash changed after acceptance"),
		}
	}
	if err := w.save(ctx, job); err != nil {
		return err
	}
	job.Status = StatusPreparing
	if err := w.save(ctx, job); err != nil {
		return err
	}
	prepared, err := w.manager.Prepare(ctx, target, *job)
	if err != nil {
		return fmt.Errorf("prepare artifact upload: %w", err)
	}
	job.ArtifactID = strings.TrimSpace(prepared.ArtifactID)
	if job.ArtifactID == "" {
		return errors.New("prepare returned no artifact_id")
	}
	if prepared.Status == PrepareStatusAvailable {
		job.Status = StatusAvailable
		job.UploadID = ""
		job.ResumableURL = ""
		job.UploadedBytes = job.SizeBytes
		job.TicketExpiresAt = ""
		job.ErrorCode = ""
		job.ErrorMessage = ""
		return w.save(ctx, job)
	}
	if prepared.Status != PrepareStatusUploadRequired || prepared.Upload == nil {
		return fmt.Errorf("prepare returned unsupported status %q", prepared.Status)
	}
	job.UploadID = prepared.Upload.UploadID
	if !prepared.Upload.ExpiresAt.IsZero() {
		job.TicketExpiresAt = prepared.Upload.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	job.Status = StatusUploading
	if err := w.save(ctx, job); err != nil {
		return err
	}
	err = w.uploader.Upload(ctx, *job, *prepared.Upload, func(sessionURL string, uploaded int64) error {
		job.ResumableURL = sessionURL
		job.UploadedBytes = uploaded
		job.Status = StatusUploading
		return w.save(ctx, job)
	})
	if err != nil {
		if errors.Is(err, ErrResumableSessionExpired) {
			job.ResumableURL = ""
			job.UploadedBytes = 0
			job.TicketExpiresAt = ""
		}
		return fmt.Errorf("upload artifact: %w", err)
	}
	job.Status = StatusCompleting
	if err := w.save(ctx, job); err != nil {
		return err
	}
	if err := w.manager.Complete(ctx, target, *job); err != nil {
		return fmt.Errorf("complete artifact upload: %w", err)
	}
	job.Status = StatusAvailable
	job.UploadedBytes = job.SizeBytes
	job.ErrorCode = ""
	job.ErrorMessage = ""
	return w.save(ctx, job)
}

func (w *Worker) save(ctx context.Context, job *daemonstore.ArtifactPublishJob) error {
	job.UpdatedAt = w.now().UTC()
	return w.store.SaveArtifactPublishJob(ctx, *job)
}

func (w *Worker) recordFailure(
	ctx context.Context,
	job *daemonstore.ArtifactPublishJob,
	err error,
) {
	var permanent permanentArtifactError
	if errors.As(err, &permanent) {
		target, targetErr := w.targets.Resolve(ctx, job.AgentID)
		if targetErr != nil {
			job.Status = StatusRetryWait
			job.ErrorCode = "failure_report_retry"
			job.ErrorMessage = errors.Join(err, targetErr).Error()
			return
		}
		if reportErr := w.manager.Fail(
			ctx, target, *job, permanent.code, err.Error(),
		); reportErr != nil {
			job.Status = StatusRetryWait
			job.ErrorCode = "failure_report_retry"
			job.ErrorMessage = errors.Join(err, reportErr).Error()
			return
		}
		job.Status = StatusFailed
		job.ErrorCode = permanent.code
	} else {
		job.Status = StatusRetryWait
		job.ErrorCode = "background_retry"
	}
	job.ErrorMessage = err.Error()
}

type permanentArtifactError struct {
	code string
	err  error
}

func (e permanentArtifactError) Error() string { return e.err.Error() }
func (e permanentArtifactError) Unwrap() error { return e.err }

func hashArtifactSnapshot(job *daemonstore.ArtifactPublishJob) error {
	file, err := os.Open(job.SpoolPath) // #nosec G304 -- path is daemon-owned durable state.
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("artifact snapshot is not a regular file")
	}
	hash := sha256.New()
	head := make([]byte, 512)
	n, readErr := io.ReadFull(file, head)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return readErr
	}
	if _, err := hash.Write(head[:n]); err != nil {
		return err
	}
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	job.SizeBytes = info.Size()
	job.SHA256 = hex.EncodeToString(hash.Sum(nil))
	job.ContentType = firstNonEmpty(
		mime.TypeByExtension(filepath.Ext(job.SourceFilename)),
		http.DetectContentType(head[:n]),
		"application/octet-stream",
	)
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
