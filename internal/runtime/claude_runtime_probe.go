package runtime

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

func fallbackClaudeRuntimeIdentity(ctx context.Context, result []byte, spec LocalACPProcessSpec) *ACPRuntimeImplementation {
	if identity := implementationIdentityFromResult(result); identity != nil && identity.Runtime != nil {
		return nil
	}
	path, err := ResolveHarnessExecutable(spec, "claude-code")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, defaultCodexRuntimeProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	prepareExecCommand(cmd)
	cmd.Cancel = func() error { return killExecCommand(cmd) }
	cmd.Dir = spec.WorkingDir
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = codexRuntimeProbeWaitDelay
	var out cappedProbeOutput
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	fields := strings.Fields(string(out.Bytes()))
	if len(fields) < 1 {
		return nil
	}
	version, ok := sanitizedCodexRuntimeVersion(fields[0])
	if !ok {
		return nil
	}
	return &ACPRuntimeImplementation{Name: "claude-code", Version: version}
}
