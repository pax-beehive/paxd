package acpcommand

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestPreferCached(t *testing.T) {
	for _, tt := range []struct {
		name          string
		command, want []string
	}{
		{"empty", nil, nil},
		{"native", []string{"claude-agent-acp"}, []string{"claude-agent-acp"}},
		{"npx fallback", []string{"npx", "-y", "@agentclientprotocol/claude-agent-acp"}, []string{"npx", "-y", "@agentclientprotocol/claude-agent-acp@*"}},
		{"npm fallback", []string{"/usr/bin/npm", "exec", "--yes", "@agentclientprotocol/claude-agent-acp"}, []string{"/usr/bin/npm", "exec", "--yes", "@agentclientprotocol/claude-agent-acp@*"}},
		{"npm alias", []string{"npm", "x", "--", "pi-acp"}, []string{"npm", "x", "--", "pi-acp@*"}},
		{"windows launcher", []string{"npx.cmd", "--yes", "@agentclientprotocol/codex-acp"}, []string{"npx.cmd", "--yes", "@agentclientprotocol/codex-acp@*"}},
		{"qwen", []string{"npx", "@qwen-code/qwen-code", "--acp"}, []string{"npx", "@qwen-code/qwen-code@*", "--acp"}},
		{"pinned", []string{"npx", "-y", "pi-acp@1.2.3"}, []string{"npx", "-y", "pi-acp@1.2.3"}},
		{"explicit latest", []string{"npx", "-y", "pi-acp@latest"}, []string{"npx", "-y", "pi-acp@latest"}},
		{"already cached", []string{"npx", "-y", "pi-acp@*"}, []string{"npx", "-y", "pi-acp@*"}},
		{"unrelated", []string{"npx", "-y", "other"}, []string{"npx", "-y", "other"}},
		{"npm install", []string{"npm", "install", "pi-acp"}, []string{"npm", "install", "pi-acp"}},
		{"npm alone", []string{"npm"}, []string{"npm"}},
		{"no package", []string{"npx", "-y"}, []string{"npx", "-y"}},
		{"unknown option value", []string{"npx", "--cache", "pi-acp"}, []string{"npx", "--cache", "pi-acp"}},
		{"adapter argument", []string{"npx", "other", "pi-acp"}, []string{"npx", "other", "pi-acp"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := append([]string(nil), tt.command...)
			got := PreferCached(tt.command)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, original, tt.command, "must not mutate persisted config")
			assert.Equal(t, got, PreferCached(got), "must be idempotent")
		})
	}
}
