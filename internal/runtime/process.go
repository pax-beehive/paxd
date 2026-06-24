package runtime

import (
	"context"
	"io"
	"os"
	"os/exec"
)

type ExecLocalACPProcessRunner struct{}

func (ExecLocalACPProcessRunner) Start(ctx context.Context, spec LocalACPProcessSpec) (LocalACPProcess, error) {
	if len(spec.Command) == 0 {
		return nil, exec.ErrNotFound
	}
	cmd := exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
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
	return &execProcess{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

type execProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr io.Reader
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
	return p.cmd.Wait()
}

func (p *execProcess) Terminate(ctx context.Context) error {
	if p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(execInterruptSignal())
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = p.cmd.Process.Kill()
		return ctx.Err()
	}
}
