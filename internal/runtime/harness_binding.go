package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveHarnessExecutable chooses the executable used by managed direct ACP
// launchers. Keep the launcher symlink: upgrades atomically switch its target.
func ResolveHarnessExecutable(spec LocalACPProcessSpec, harness string) (string, error) {
	key, name := "", ""
	switch harness {
	case "codex":
		key, name = "CODEX_PATH", "codex"
	case "claude-code":
		key, name = "CLAUDE_CODE_EXECUTABLE", "claude"
	default:
		return "", fmt.Errorf("harness %s does not use an external ACP executable", harness)
	}
	value, set := environmentOverride(spec.Env, key)
	if !set {
		value = os.Getenv(key)
	}
	if value == "" {
		value = name
	}
	path, err := resolveACPExecutable(spec, value)
	if err != nil {
		return "", fmt.Errorf("resolve %s for ACP (set %s to an installed executable): %w", name, key, err)
	}
	return path, nil
}

func directHarnessCommand(command []string) (string, string) {
	if len(command) == 0 {
		return "", ""
	}
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(command[0])), ".cmd") {
	case "codex-acp":
		return "codex", "CODEX_PATH"
	case "claude-agent-acp":
		return "claude-code", "CLAUDE_CODE_EXECUTABLE"
	case "pi-acp":
		return "pi", "PI_ACP_SDK_ROOT"
	default:
		return "", ""
	}
}

func bindHarnessExecutable(spec LocalACPProcessSpec) (LocalACPProcessSpec, error) {
	harness, key := directHarnessCommand(spec.Command)
	if harness == "" {
		return spec, nil
	}
	var path string
	var err error
	if harness == "pi" {
		path, _, err = ResolvePiSDKBinding(spec)
	} else {
		path, err = ResolveHarnessExecutable(spec, harness)
	}
	if err != nil {
		return spec, err
	}
	env := make(map[string]string, len(spec.Env)+1)
	for k, v := range spec.Env {
		env[k] = v
	}
	env[key] = path
	spec.Env = env
	return spec, nil
}
