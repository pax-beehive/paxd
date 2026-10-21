package cronjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

type Service struct {
	db             *gorm.DB
	now            func() time.Time
	runner         Runner
	executor       SessionExecutor
	validateTarget func(context.Context, *Job) error
	lockPath       string
	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
}

func New(db *gorm.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("cron database is required")
	}
	if err := db.AutoMigrate(&Job{}, &Run{}, &Session{}, &SessionEvent{}, &Envelope{}); err != nil {
		return nil, fmt.Errorf("migrate cron jobs: %w", err)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{db: db, now: opts.Now, runner: opts.Runner, executor: opts.Executor, validateTarget: opts.ValidateTarget, lockPath: opts.LockPath}, nil
}

func (s *Service) Create(ctx context.Context, req *CreateRequest) (*Job, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: job is required", ErrInvalidArgument)
	}
	job := &Job{
		ConnectionID: req.ConnectionID, SessionMode: req.SessionMode,
		Name: req.Name, Agent: req.Agent, Prompt: req.Prompt, WorkingDir: req.WorkingDir,
		Schedule: req.Schedule, Timezone: req.Timezone, TimeoutSeconds: req.TimeoutSeconds,
		Enabled: req.Enabled == nil || *req.Enabled, Revision: 1,
	}
	if job.SessionMode == "" {
		job.SessionMode = "new"
	}
	if s.validateTarget != nil {
		if err := s.validateTarget(ctx, job); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		}
	}
	if job.Timezone == "" {
		job.Timezone = "UTC"
	}
	if job.TimeoutSeconds == 0 {
		job.TimeoutSeconds = 3600
	}
	if err := validateJob(job); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if err := setNextRun(job, s.now()); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	id, err := newID("cron_")
	if err != nil {
		return nil, err
	}
	job.ID, job.CreatedAt, job.UpdatedAt = id, s.now().UTC(), s.now().UTC()
	if err := s.db.WithContext(ctx).Create(job).Error; err != nil {
		return nil, fmt.Errorf("create cron job: %w", err)
	}
	return job, nil
}

func (s *Service) Get(ctx context.Context, id string) (*Job, error) {
	var job Job
	if err := s.db.WithContext(ctx).First(&job, "id = ?", id).Error; err != nil {
		return nil, lookupError(err)
	}
	return &job, nil
}

func (s *Service) List(ctx context.Context, agent string) ([]Job, error) {
	jobs := []Job{}
	query := s.db.WithContext(ctx).Order("created_at ASC, id ASC")
	if agent != "" {
		query = query.Where("agent = ?", agent)
	}
	err := query.Find(&jobs).Error
	return jobs, err
}

func (s *Service) Update(ctx context.Context, id string, req *UpdateRequest) (*Job, error) {
	if req == nil || req.ExpectedRevision == 0 {
		return nil, fmt.Errorf("%w: expected_revision is required", ErrInvalidArgument)
	}
	job, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if job.Revision != req.ExpectedRevision {
		return nil, ErrConflict
	}
	targetChanged := (req.ConnectionID != nil && *req.ConnectionID != job.ConnectionID) || (req.WorkingDir != nil && *req.WorkingDir != job.WorkingDir) || (req.SessionMode != nil && *req.SessionMode != job.SessionMode)
	if targetChanged && job.ActiveRunID != "" {
		return nil, ErrConflict
	}
	if req.ResetSession {
		if job.ActiveRunID != "" {
			return nil, ErrConflict
		}
		job.BoundSessionID = ""
	}
	if job.BoundSessionID != "" && ((req.ConnectionID != nil && *req.ConnectionID != job.ConnectionID) || (req.WorkingDir != nil && *req.WorkingDir != job.WorkingDir) || (req.SessionMode != nil && *req.SessionMode != job.SessionMode)) {
		return nil, fmt.Errorf("%w: reset_session is required to change a bound session target", ErrInvalidArgument)
	}
	applyUpdate(job, req)
	if s.validateTarget != nil && (targetChanged || req.Agent != nil) {
		if err := s.validateTarget(ctx, job); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		}
	}
	if err := validateJob(job); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	if req.Schedule != nil || req.Timezone != nil || req.Enabled != nil {
		if err := setNextRun(job, s.now()); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
		}
	}
	job.Revision++
	job.UpdatedAt = s.now().UTC()
	// Never overwrite the worker's reservation with the API's earlier snapshot.
	result := s.db.WithContext(ctx).Model(&Job{}).Where("id = ? AND revision = ?", id, req.ExpectedRevision).
		Select("agent_id", "remote_id", "cloud_api_url", "connection_id", "session_mode", "bound_session_id", "name", "agent", "prompt", "working_dir", "schedule", "timezone", "enabled", "timeout_seconds", "next_run_at", "revision", "updated_at").Updates(job)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected != 1 {
		return nil, ErrConflict
	}
	return s.Get(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	result := s.db.WithContext(ctx).Delete(&Job{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Service) Runs(ctx context.Context, jobID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	runs := []Run{}
	query := s.db.WithContext(ctx).Order("scheduled_at DESC, id DESC").Limit(limit)
	if jobID != "" {
		query = query.Where("job_id = ?", jobID)
	}
	err := query.Find(&runs).Error
	return runs, err
}

func applyUpdate(job *Job, req *UpdateRequest) {
	for target, value := range map[*string]*string{
		&job.ConnectionID: req.ConnectionID, &job.SessionMode: req.SessionMode,
		&job.Name: req.Name, &job.Agent: req.Agent, &job.Prompt: req.Prompt,
		&job.WorkingDir: req.WorkingDir, &job.Schedule: req.Schedule, &job.Timezone: req.Timezone,
	} {
		if value != nil {
			*target = *value
		}
	}
	if req.Enabled != nil {
		job.Enabled = *req.Enabled
	}
	if req.TimeoutSeconds != nil {
		job.TimeoutSeconds = *req.TimeoutSeconds
	}
}

func validateJob(job *Job) error {
	job.Name, job.Agent = strings.TrimSpace(job.Name), strings.TrimSpace(job.Agent)
	if job.Name == "" || len(job.Name) > 200 {
		return errors.New("name must contain between 1 and 200 bytes")
	}
	if strings.TrimSpace(job.ConnectionID) == "" {
		return errors.New("connection_id is required")
	}
	if job.SessionMode != "new" && job.SessionMode != "reuse" {
		return errors.New("session_mode must be new or reuse")
	}

	if strings.TrimSpace(job.Prompt) == "" || len(job.Prompt) > 128*1024 {
		return errors.New("prompt must contain between 1 and 131072 bytes")
	}
	if !filepath.IsAbs(job.WorkingDir) {
		return errors.New("working_dir must be an absolute directory")
	}
	info, err := os.Stat(job.WorkingDir)
	if err != nil || !info.IsDir() {
		return errors.New("working_dir must be an existing directory")
	}
	if job.TimeoutSeconds < 1 || job.TimeoutSeconds > 86400 {
		return errors.New("timeout_seconds must be between 1 and 86400")
	}
	_, err = nextRun(job, time.Now())
	return err
}

func nextRun(job *Job, after time.Time) (time.Time, error) {
	if len(strings.Fields(job.Schedule)) != 5 {
		return time.Time{}, errors.New("schedule must be a five-field cron expression")
	}
	if job.Timezone == "" || strings.ContainsAny(job.Timezone, " \t\r\n") {
		return time.Time{}, errors.New("timezone must be an IANA timezone name")
	}
	if _, err := time.LoadLocation(job.Timezone); err != nil {
		return time.Time{}, fmt.Errorf("invalid timezone: %w", err)
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse("CRON_TZ=" + job.Timezone + " " + job.Schedule)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid schedule: %w", err)
	}
	next := schedule.Next(after).UTC()
	if next.IsZero() {
		return time.Time{}, errors.New("schedule has no upcoming occurrence")
	}
	return next, nil
}

func setNextRun(job *Job, now time.Time) error {
	job.NextRunAt = nil
	if job.Enabled {
		next, err := nextRun(job, now)
		if err != nil {
			return err
		}
		job.NextRunAt = &next
	}
	return nil
}

func newID(prefix string) (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(data[:]), nil
}

func lookupError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
