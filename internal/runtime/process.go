package runtime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	defaultExecTerminateGracePeriod = 5 * time.Second
	execTerminateReapTimeout        = 2 * time.Second
)

type ExecLocalACPProcessRunner struct {
	TerminateGracePeriod time.Duration
}

func (r ExecLocalACPProcessRunner) Start(ctx context.Context, spec LocalACPProcessSpec) (LocalACPProcess, error) {
	if len(spec.Command) == 0 {
		return nil, exec.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Terminate owns the process lifetime. CommandContext kills only the process
	// leader on cancellation, which can orphan ACP server descendants.
	// Resolve with the same PATH and cwd that installation inspection observes.
	// exec.Command's implicit lookup happens before cmd.Env and cmd.Dir are set.
	executable, err := resolveACPExecutable(spec, spec.Command[0])
	if err != nil {
		return nil, fmt.Errorf("resolve ACP executable %s: %w", spec.Command[0], err)
	}
	spec, err = bindHarnessExecutable(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(executable, spec.Command[1:]...)
	prepareExecCommand(cmd)
	cmd.Dir = spec.WorkingDir
	cmd.Env = os.Environ()
	for key, value := range spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	gracePeriod := r.TerminateGracePeriod
	if gracePeriod <= 0 {
		gracePeriod = defaultExecTerminateGracePeriod
	}
	return &execProcess{
		cmd:                  cmd,
		stdin:                stdin,
		stdout:               stdout,
		stderr:               stderr,
		terminateGracePeriod: gracePeriod,
		waitDone:             make(chan struct{}),
	}, nil
}

type execProcess struct {
	cmd                  *exec.Cmd
	stdin                io.WriteCloser
	stdout               io.Reader
	stderr               io.Reader
	terminateGracePeriod time.Duration
	waitOnce             sync.Once
	waitDone             chan struct{}
	waitErr              error
}

func (p *execProcess) Stdin() io.WriteCloser {
	return p.stdin
}

func (p *execProcess) Stdout() io.Reader {
	return p.stdout
}

func (p *execProcess) Stderr() io.Reader {
	return p.stderr
}

func (p *execProcess) Wait() error {
	p.startWait()
	<-p.waitDone
	return p.waitErr
}

func (p *execProcess) startWait() {
	p.waitOnce.Do(func() {
		go func() {
			p.waitErr = p.cmd.Wait()
			// The ACP leader can exit while descendants remain alive and keep
			// resources such as inherited pipes open.
			_ = killExecCommand(p.cmd)
			close(p.waitDone)
		}()
	})
}

func (p *execProcess) Terminate(ctx context.Context) error {
	if p.cmd.Process == nil {
		return nil
	}
	p.startWait()
	select {
	case <-p.waitDone:
		return p.waitErr
	default:
	}

	_ = interruptExecCommand(p.cmd)

	graceCtx, cancel := context.WithTimeout(ctx, p.terminateGracePeriod)
	defer cancel()
	select {
	case <-p.waitDone:
		return p.waitErr
	case <-graceCtx.Done():
	}

	_ = killExecCommand(p.cmd)
	reapTimer := time.NewTimer(execTerminateReapTimeout)
	defer reapTimer.Stop()
	select {
	case <-p.waitDone:
		return nil
	case <-reapTimer.C:
		return graceCtx.Err()
	}
}
