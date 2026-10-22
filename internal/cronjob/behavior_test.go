package cronjob_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cronjob"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T, options cronjob.Options) (*cronjob.Service, *daemonstore.Store) {
	t.Helper()
	db, err := daemonstore.OpenSQLite(filepath.Join(t.TempDir(), "paxd.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := cronjob.New(db.DB(), options)
	require.NoError(t, err)
	return svc, db
}

func createTestJob(t *testing.T, svc *cronjob.Service) *cronjob.Job {
	t.Helper()
	job, err := svc.Create(context.Background(), &cronjob.CreateRequest{ConnectionID: "conn_test",
		Name: "Review", Agent: "codex", Prompt: "Review.", WorkingDir: t.TempDir(), Schedule: "* * * * *",
	})
	require.NoError(t, err)
	return job
}

func TestUpdatesValidateBeforeWritingAndRejectStaleRevisions(t *testing.T) {
	svc, _ := newTestService(t, cronjob.Options{})
	job := createTestJob(t, svc)
	ctx := context.Background()
	badSchedule := "every minute"
	_, err := svc.Update(ctx, job.ID, &cronjob.UpdateRequest{Schedule: &badSchedule, ExpectedRevision: job.Revision})
	require.ErrorIs(t, err, cronjob.ErrInvalidArgument)
	loaded, err := svc.Get(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, job.Revision, loaded.Revision)
	require.Equal(t, job.Schedule, loaded.Schedule)
	enabled := false
	paused, err := svc.Update(ctx, job.ID, &cronjob.UpdateRequest{Enabled: &enabled, ExpectedRevision: job.Revision})
	require.NoError(t, err)
	require.Nil(t, paused.NextRunAt)
	_, err = svc.Update(ctx, job.ID, &cronjob.UpdateRequest{Enabled: &enabled, ExpectedRevision: job.Revision})
	require.ErrorIs(t, err, cronjob.ErrConflict)
	enabled = true
	resumed, err := svc.Update(ctx, job.ID, &cronjob.UpdateRequest{Enabled: &enabled, ExpectedRevision: paused.Revision})
	require.NoError(t, err)
	require.NotNil(t, resumed.NextRunAt)
	require.Equal(t, job.Prompt, resumed.Prompt)
}

type blockingRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingRunner) Run(ctx context.Context, _ *cronjob.Job) (string, error) {
	close(r.started)
	select {
	case <-r.release:
		return "done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestActiveRunBlocksManualRunsAndSkipsOverlappingOccurrences(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	svc, _ := newTestService(t, cronjob.Options{Now: func() time.Time { return now }, Runner: runner})
	job := createTestJob(t, svc)
	_, err := svc.RunNow(ctx, job.ID)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := svc.ExecuteNext(ctx); done <- err }()
	select {
	case <-runner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not start")
	}
	_, err = svc.RunNow(ctx, job.ID)
	require.ErrorIs(t, err, cronjob.ErrConflict)
	now = now.Add(time.Minute)
	require.NoError(t, svc.QueueDue(ctx))
	ran, err := svc.ExecuteNext(ctx)
	require.NoError(t, err)
	require.False(t, ran)
	close(runner.release)
	require.NoError(t, <-done)
	runs, err := svc.Runs(ctx, job.ID, 10)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	statuses := []string{runs[0].Status, runs[1].Status}
	require.ElementsMatch(t, []string{"skipped", "succeeded"}, statuses)
}

func TestRestartDoesNotRetryInterruptedWorkAndRetainsQueuedWork(t *testing.T) {
	ctx := context.Background()
	runner := &recordingRunner{}
	svc, db := newTestService(t, cronjob.Options{Runner: runner})
	job := createTestJob(t, svc)
	run, err := svc.RunNow(ctx, job.ID)
	require.NoError(t, err)
	// Simulate a crash after reserving the run but before recording its outcome.
	require.NoError(t, db.DB().Model(&cronjob.Run{}).Where("id = ?", run.ID).Update("status", "running").Error)
	second := createTestJob(t, svc)
	_, err = svc.RunNow(ctx, second.ID)
	require.NoError(t, err)
	require.NoError(t, svc.RecoverInterrupted(ctx))
	ran, err := svc.ExecuteNext(ctx)
	require.NoError(t, err)
	require.True(t, ran)
	ran, err = svc.ExecuteNext(ctx)
	require.NoError(t, err)
	require.False(t, ran)
	require.Equal(t, 1, runner.calls)
	runs, err := svc.Runs(ctx, job.ID, 10)
	require.NoError(t, err)
	require.Equal(t, "interrupted", runs[0].Status)
	job, err = svc.Get(ctx, job.ID)
	require.NoError(t, err)
	require.Empty(t, job.ActiveRunID)
}

func TestPausedAndDeletedJobsDoNotExecuteQueuedSchedules(t *testing.T) {
	for _, action := range []string{"pause", "delete"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			runner := &recordingRunner{}
			svc, _ := newTestService(t, cronjob.Options{Now: func() time.Time { return now }, Runner: runner})
			job := createTestJob(t, svc)
			now = now.Add(2 * time.Minute)
			require.NoError(t, svc.QueueDue(ctx))
			job, err := svc.Get(ctx, job.ID)
			require.NoError(t, err)
			if action == "pause" {
				enabled := false
				_, err = svc.Update(ctx, job.ID, &cronjob.UpdateRequest{Enabled: &enabled, ExpectedRevision: job.Revision})
			} else {
				err = svc.Delete(ctx, job.ID)
			}
			require.NoError(t, err)
			_, err = svc.ExecuteNext(ctx)
			require.NoError(t, err)
			require.Zero(t, runner.calls)
			runs, err := svc.Runs(ctx, job.ID, 10)
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.Equal(t, "skipped", runs[0].Status)
		})
	}
}

func TestOnlyOneSchedulerCanOwnDatabase(t *testing.T) {
	options := cronjob.Options{Runner: &recordingRunner{}, LockPath: filepath.Join(t.TempDir(), "scheduler.lock")}
	first, db := newTestService(t, options)
	second, err := cronjob.New(db.DB(), options)
	require.NoError(t, err)
	require.NoError(t, first.Start(context.Background()))
	require.Error(t, second.Start(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, first.Shutdown(ctx))
	require.False(t, first.Running())
	require.NoError(t, second.Start(context.Background()))
	require.NoError(t, second.Shutdown(ctx))
}
