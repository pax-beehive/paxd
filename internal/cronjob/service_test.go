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

func TestJobsPersistAndUseTheConfiguredTimezone(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "paxd.db")
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	db, err := daemonstore.OpenSQLite(path)
	require.NoError(t, err)
	svc, err := cronjob.New(db.DB(), cronjob.Options{Now: func() time.Time { return now }})
	require.NoError(t, err)
	job, err := svc.Create(ctx, &cronjob.CreateRequest{ConnectionID: "conn_test",
		Name: "Morning review", Agent: "codex", Prompt: "Review the repository.",
		WorkingDir: t.TempDir(), Schedule: "0 9 * * *", Timezone: "America/Los_Angeles",
	})
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC), *job.NextRunAt)
	require.True(t, job.Enabled)
	require.NoError(t, db.Close())
	db, err = daemonstore.OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err = cronjob.New(db.DB(), cronjob.Options{Now: func() time.Time { return now }})
	require.NoError(t, err)
	loaded, err := svc.Get(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, job.Prompt, loaded.Prompt)
	require.Equal(t, job.NextRunAt, loaded.NextRunAt)
	jobs, err := svc.List(ctx, "codex")
	require.NoError(t, err)
	require.Len(t, jobs, 1)
}

type recordingRunner struct{ calls int }

func (r *recordingRunner) Run(_ context.Context, job *cronjob.Job) (string, error) {
	r.calls++
	return "Completed: " + job.Prompt, nil
}

func TestDueJobsRunOnceAndCoalesceMissedOccurrences(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	db, err := daemonstore.OpenSQLite(filepath.Join(t.TempDir(), "paxd.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runner := &recordingRunner{}
	svc, err := cronjob.New(db.DB(), cronjob.Options{Now: func() time.Time { return now }, Runner: runner})
	require.NoError(t, err)
	job, err := svc.Create(ctx, &cronjob.CreateRequest{ConnectionID: "conn_test",
		Name: "Review", Agent: "codex", Prompt: "Review.", WorkingDir: t.TempDir(),
		Schedule: "* * * * *", Timezone: "UTC",
	})
	require.NoError(t, err)
	now = now.Add(10 * time.Minute)
	require.NoError(t, svc.QueueDue(ctx))
	require.NoError(t, svc.QueueDue(ctx))
	ran, err := svc.ExecuteNext(ctx)
	require.NoError(t, err)
	require.True(t, ran)
	ran, err = svc.ExecuteNext(ctx)
	require.NoError(t, err)
	require.False(t, ran)
	require.Equal(t, 1, runner.calls)
	runs, err := svc.Runs(ctx, job.ID, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "succeeded", runs[0].Status)
	require.Contains(t, runs[0].Output, "Completed")
	job, err = svc.Get(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute), *job.NextRunAt)
	require.Empty(t, job.ActiveRunID)
}
