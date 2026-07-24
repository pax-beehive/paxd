package runtime

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecLocalACPProcessRunnerStartsProcessAndExposesPipes(t *testing.T) {
	runner := ExecLocalACPProcessRunner{}
	proc, err := runner.Start(context.Background(), LocalACPProcessSpec{
		Command: []string{"/bin/sh", "-c", "cat; printf err >&2"},
		Env:     map[string]string{"PAXD_RUNTIME_TEST": "1"},
	})
	require.NoError(t, err)

	_, err = proc.Stdin().Write([]byte("hello\n"))
	require.NoError(t, err)
	require.NoError(t, proc.Stdin().Close())

	stdout, err := io.ReadAll(proc.Stdout())
	require.NoError(t, err)
	stderr, err := io.ReadAll(proc.Stderr())
	require.NoError(t, err)
	require.NoError(t, proc.Wait())
	assert.Equal(t, "hello\n", string(stdout))
	assert.Equal(t, "err", string(stderr))
}

func TestExecLocalACPProcessRunnerReportsMissingCommand(t *testing.T) {
	_, err := ExecLocalACPProcessRunner{}.Start(context.Background(), LocalACPProcessSpec{})
	require.ErrorContains(t, err, exec.ErrNotFound.Error())
}

func TestExecProcessTerminateStopsRunningProcess(t *testing.T) {
	proc, err := ExecLocalACPProcessRunner{}.Start(context.Background(), LocalACPProcessSpec{
		Command: []string{"/bin/sh", "-c", "sleep 5"},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = proc.Terminate(ctx)
	assert.NoError(t, ctx.Err())
	assert.NotErrorIs(t, err, context.DeadlineExceeded)
}

func TestExecProcessWaitIsConcurrentAndIdempotent(t *testing.T) {
	proc, err := ExecLocalACPProcessRunner{}.Start(context.Background(), LocalACPProcessSpec{
		Command: []string{"/bin/sh", "-c", "exit 7"},
	})
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- proc.Wait()
		}()
	}
	wg.Wait()
	close(errs)

	for waitErr := range errs {
		var exitErr *exec.ExitError
		require.ErrorAs(t, waitErr, &exitErr)
		assert.Equal(t, 7, exitErr.ExitCode())
	}

	var exitErr *exec.ExitError
	require.ErrorAs(t, proc.Wait(), &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
}
