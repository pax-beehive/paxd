// Package acpcommand controls built-in ACP package launcher defaults.
package acpcommand

import (
	"path/filepath"
	"strings"
)

// PreferCached accepts any installed version of an unversioned built-in ACP
// fallback. npm exec treats a bare package name as a request to check for newer
// releases, whereas an explicit @* accepts the existing npx installation.
// Missing packages can still be installed. Explicit versions and tags are kept.
// The returned command does not change the caller's persisted configuration.
func PreferCached(command []string) []string {
	if len(command) == 0 {
		return command
	}
	launcher := strings.TrimSuffix(strings.ToLower(filepath.Base(command[0])), ".cmd")
	i := 1
	switch launcher {
	case "npx":
	case "npm":
		if len(command) < 2 || (command[1] != "exec" && command[1] != "x") {
			return command
		}
		i = 2
	default:
		return command
	}
	for i < len(command) && (command[i] == "-y" || command[i] == "--yes") {
		i++
	}
	if i < len(command) && command[i] == "--" {
		i++
	}
	if i == len(command) {
		return command
	}
	switch command[i] {
	case "@agentclientprotocol/claude-agent-acp", "@agentclientprotocol/codex-acp", "pi-acp", "@qwen-code/qwen-code":
		result := append([]string(nil), command...)
		result[i] += "@*"
		return result
	default:
		return command
	}
}
