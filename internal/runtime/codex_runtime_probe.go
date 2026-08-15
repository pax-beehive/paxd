package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultCodexRuntimeProbeTimeout = 2 * time.Second
	codexRuntimeProbeWaitDelay      = 250 * time.Millisecond
	maxCodexRuntimeProbeOutputBytes = 4 * 1024
	maxCodexRuntimeVersionRunes     = 128
)

var errCodexRuntimeProbeUnavailable = errors.New("codex runtime version is unavailable")

// CodexRuntimeVersionProber is the narrow seam used to discover the Codex CLI
// version behind the known Codex ACP adapter. Implementations return only a
// version candidate; callers sanitize it and supply the fixed runtime name.
type CodexRuntimeVersionProber interface {
	ProbeCodexRuntimeVersion(context.Context, LocalACPProcessSpec) (string, error)
}

// ExecCodexRuntimeVersionProber invokes the fixed, non-configurable
// `codex --version` command. ACP command arguments and initialize metadata are
// never interpolated into the probe command.
type ExecCodexRuntimeVersionProber struct {
	Timeout time.Duration
}

func (p ExecCodexRuntimeVersionProber) ProbeCodexRuntimeVersion(
	ctx context.Context,
	spec LocalACPProcessSpec,
) (string, error) {
	if !isTrustedCodexACPCommand(spec.Command) {
		return "", errCodexRuntimeProbeUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultCodexRuntimeProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	executable, err := resolveCodexProbeExecutable(spec)
	if err != nil {
		return "", errCodexRuntimeProbeUnavailable
	}
	cmd := exec.CommandContext(probeCtx, executable, "--version") // #nosec G204 -- path is resolved from the ACP process PATH and arguments are fixed.
	prepareExecCommand(cmd)
	cmd.Cancel = func() error { return killExecCommand(cmd) }
	cmd.WaitDelay = codexRuntimeProbeWaitDelay
	cmd.Dir = spec.WorkingDir
	cmd.Env = os.Environ()
	for key, value := range spec.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout cappedProbeOutput
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", errCodexRuntimeProbeUnavailable
	}
	return parseCodexCLIVersion(stdout.Bytes())
}

func resolveCodexProbeExecutable(spec LocalACPProcessSpec) (string, error) {
	pathValue, overridden := environmentOverride(spec.Env, "PATH")
	if !overridden {
		pathValue = os.Getenv("PATH")
	}
	if pathValue == "" {
		return "", errCodexRuntimeProbeUnavailable
	}
	workingDir := strings.TrimSpace(spec.WorkingDir)
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return "", errCodexRuntimeProbeUnavailable
		}
	}
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return "", errCodexRuntimeProbeUnavailable
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = absWorkingDir
		} else if !filepath.IsAbs(directory) {
			directory = filepath.Join(absWorkingDir, directory)
		}
		for _, name := range codexProbeExecutableNames(spec.Env) {
			candidate := filepath.Join(directory, name)
			info, statErr := os.Stat(candidate)
			if statErr != nil || !codexProbeFileIsExecutable(info) {
				continue
			}
			return candidate, nil
		}
	}
	return "", errCodexRuntimeProbeUnavailable
}

func fallbackCodexRuntimeIdentity(
	ctx context.Context,
	result []byte,
	spec LocalACPProcessSpec,
	prober CodexRuntimeVersionProber,
) *ACPRuntimeImplementation {
	if prober == nil || !isTrustedCodexACPCommand(spec.Command) {
		return nil
	}
	if identity := implementationIdentityFromResult(result); identity != nil && identity.Runtime != nil {
		return nil
	}
	version, err := prober.ProbeCodexRuntimeVersion(ctx, spec)
	if err != nil {
		return nil
	}
	version, ok := sanitizedCodexRuntimeVersion(version)
	if !ok {
		return nil
	}
	return &ACPRuntimeImplementation{Name: "codex", Version: version}
}

func implementationIdentityWithRuntimeFallback(
	result []byte,
	fallback *ACPRuntimeImplementation,
) *ACPImplementationIdentity {
	identity := implementationIdentityFromResult(result)
	if fallback == nil || (identity != nil && identity.Runtime != nil) {
		return identity
	}
	runtime := *fallback
	if identity == nil {
		identity = &ACPImplementationIdentity{}
	}
	identity.Runtime = &runtime
	identity.IdentityFingerprint = implementationIdentityFingerprint(identity.ACPAgent, identity.Runtime)
	return identity
}

func isTrustedCodexACPCommand(command []string) bool {
	if len(command) == 0 {
		return false
	}
	executable := strings.ToLower(strings.TrimSpace(filepath.Base(command[0])))
	executable = strings.TrimSuffix(executable, ".cmd")
	if executable == "codex-acp" {
		return true
	}
	if executable != "npx" {
		return false
	}
	args := command[1:]
	for len(args) > 0 && (args[0] == "-y" || args[0] == "--yes") {
		args = args[1:]
	}
	return len(args) > 0 && args[0] == "@agentclientprotocol/codex-acp"
}

func parseCodexCLIVersion(output []byte) (string, error) {
	fields := strings.Fields(string(bytes.TrimSpace(output)))
	if len(fields) != 2 || fields[0] != "codex-cli" {
		return "", errCodexRuntimeProbeUnavailable
	}
	version, ok := sanitizedCodexRuntimeVersion(fields[1])
	if !ok {
		return "", errCodexRuntimeProbeUnavailable
	}
	return version, nil
}

func sanitizedCodexRuntimeVersion(value string) (string, bool) {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) == 0 || len(runes) > maxCodexRuntimeVersionRunes {
		return "", false
	}
	if !isASCIIAlphaNumeric(runes[0]) {
		return "", false
	}
	for _, r := range runes {
		if isASCIIAlphaNumeric(r) {
			continue
		}
		switch r {
		case '.', '-', '+', '_':
			continue
		default:
			return "", false
		}
	}
	return value, true
}

func isASCIIAlphaNumeric(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

type cappedProbeOutput struct {
	buf bytes.Buffer
}

func (o *cappedProbeOutput) Write(payload []byte) (int, error) {
	written := len(payload)
	remaining := maxCodexRuntimeProbeOutputBytes - o.buf.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			payload = payload[:remaining]
		}
		_, _ = o.buf.Write(payload)
	}
	return written, nil
}

func (o *cappedProbeOutput) Bytes() []byte {
	return o.buf.Bytes()
}
