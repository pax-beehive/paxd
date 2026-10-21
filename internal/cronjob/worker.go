package cronjob

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// QueueDue coalesces downtime into one occurrence and advances the cursor in
// the same transaction as the durable run reservation.
func (s *Service) QueueDue(ctx context.Context) error {
	var due []Job
	if err := s.db.WithContext(ctx).Where("enabled = ? AND next_run_at <= ?", true, s.now().UTC()).
		Order("next_run_at ASC").Limit(100).Find(&due).Error; err != nil {
		return err
	}
	for _, job := range due {
		if _, err := s.queue(ctx, job.ID, "scheduled"); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s *Service) RunNow(ctx context.Context, id string) (*Run, error) {
	return s.queue(ctx, id, "manual")
}

func (s *Service) queue(ctx context.Context, id, trigger string) (*Run, error) {
	var run *Run
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job Job
		if err := tx.First(&job, "id = ?", id).Error; err != nil {
			return lookupError(err)
		}
		now := s.now().UTC()
		if trigger == "scheduled" && (!job.Enabled || job.NextRunAt == nil || job.NextRunAt.After(now)) {
			return nil
		}
		if trigger == "manual" && job.ActiveRunID != "" {
			return ErrConflict
		}
		newRunID, err := newID("run_")
		if err != nil {
			return err
		}
		run = &Run{Prompt: job.Prompt, WorkingDir: job.WorkingDir, ConnectionID: job.ConnectionID, SessionMode: job.SessionMode, ID: newRunID, JobID: job.ID, Trigger: trigger, Status: "queued", ScheduledAt: now}
		if trigger == "scheduled" {
			run.ScheduledAt = *job.NextRunAt
			if err := setNextRun(&job, now); err != nil {
				return err
			}
		}
		if job.ActiveRunID != "" {
			run.Status, run.Error, run.FinishedAt = "skipped", "Previous run is still active.", &now
		} else {
			job.ActiveRunID = run.ID
		}
		job.Revision++
		job.UpdatedAt = now
		data, err := json.Marshal(job)
		if err != nil {
			return err
		}
		run.JobJSON = string(data)
		if err := tx.Create(run).Error; err != nil {
			return err
		}
		return tx.Save(&job).Error
	})
	return run, err
}

// ExecuteNext reserves one queued run before invoking the agent. Multiple
// workers may call it, but a run can move from queued to running only once.
func (s *Service) ExecuteNext(ctx context.Context) (bool, error) {
	if s.runner == nil && s.executor == nil {
		return false, errors.New("cron runner is not configured")
	}
	run, err := s.claimNext(ctx)
	if err != nil || run == nil {
		return false, err
	}
	var job Job
	if err := json.Unmarshal([]byte(run.JobJSON), &job); err != nil {
		return true, s.finish(run, "failed", "", "Invalid stored job snapshot.")
	}
	current, err := s.Get(ctx, job.ID)
	if errors.Is(err, ErrNotFound) || (err == nil && !current.Enabled && run.Trigger == "scheduled") {
		return true, s.finish(run, "skipped", "", "Job was deleted or paused before execution.")
	}
	if err != nil {
		return true, s.finish(run, "failed", "", err.Error())
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(job.TimeoutSeconds)*time.Second)
	var output string
	var runErr error
	if s.executor != nil {
		output, runErr = s.executor.Execute(runCtx, &job, run)
	} else {
		output, runErr = s.runner.Run(runCtx, &job)
	}
	ctxErr := runCtx.Err()
	cancel()
	status, detail := "succeeded", ""
	if runErr != nil {
		status, detail = "failed", runErr.Error()
	}
	if ctxErr != nil {
		status, detail = "interrupted", ctxErr.Error()
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			status = "failed"
		}
	}
	return true, s.finish(run, status, output, detail)
}

func (s *Service) claimNext(ctx context.Context) (*Run, error) {
	var run Run
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("status = ?", "queued").Order("scheduled_at ASC, id ASC").First(&run).Error; err != nil {
			return err
		}
		now := s.now().UTC()
		result := tx.Model(&Run{}).Where("id = ? AND status = ?", run.ID, "queued").
			Updates(map[string]any{"status": "running", "started_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		run.Status, run.StartedAt = "running", &now
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, ErrConflict) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func (s *Service) finish(run *Run, status, output, detail string) error {
	// A cancelled agent context must not prevent recording its final outcome.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Run{}).Where("id = ? AND status = ?", run.ID, "running").Updates(map[string]any{
			"status": status, "finished_at": s.now().UTC(), "output": truncate(output, 256*1024), "error": truncate(detail, 8192),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrConflict
		}
		return tx.Model(&Job{}).Where("id = ? AND active_run_id = ?", run.JobID, run.ID).
			Updates(map[string]any{"active_run_id": "", "revision": gorm.Expr("revision + 1")}).Error
	})
	if err != nil {
		return fmt.Errorf("persist cron run result: %w", err)
	}
	return nil
}

// RecoverInterrupted is called only by the exclusive scheduler owner at boot.
// Queued work survives; work that might have produced side effects is not retried.
func (s *Service) RecoverInterrupted(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var runs []Run
		if err := tx.Where("status = ?", "running").Find(&runs).Error; err != nil {
			return err
		}
		for _, run := range runs {
			if err := tx.Model(&Job{}).Where("id = ? AND active_run_id = ?", run.JobID, run.ID).
				Updates(map[string]any{"active_run_id": "", "revision": gorm.Expr("revision + 1")}).Error; err != nil {
				return err
			}
		}
		return tx.Model(&Run{}).Where("status = ?", "running").Updates(map[string]any{
			"status": "interrupted", "finished_at": s.now().UTC(), "error": "Daemon stopped before the run completed; not retried.",
		}).Error
	})
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n[truncated]"
}
