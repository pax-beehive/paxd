package artifactfetch_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactfetch"
)

func TestCheckPathGivenTableOfPathsThenAppliesRulesInOrder(t *testing.T) {
	const home = "/home/me"
	cases := []struct {
		name string
		path string
		code string
		rule string
	}{
		{"ordinary file", "/home/me/proj/notes.md", "", ""},
		{"cleaned inside home", "/home/me/proj/../proj/./notes.md", "", ""},
		{"allowlisted .claude segment", "/home/me/.claude/notes.md", "", ""},
		{"allowlisted case-insensitive", "/home/me/.Codex/history.md", "", ""},
		{"allowlisted .agents segment", "/home/me/proj/.agents/plan.md", "", ""},
		{"allowlisted .git segment", "/home/me/proj/.git/HEAD", "", ""},
		{"public key with unrelated name", "/home/me/proj/key-notes.md", "", ""},

		{"empty", "", artifactfetch.CodeInvalidPath, artifactfetch.RuleHomeRoot},
		{"relative", "proj/notes.md", artifactfetch.CodeInvalidPath, artifactfetch.RuleHomeRoot},
		{"nul byte", "/home/me/a\x00b", artifactfetch.CodeInvalidPath, artifactfetch.RuleHomeRoot},
		{"newline", "/home/me/a\nb", artifactfetch.CodeInvalidPath, artifactfetch.RuleHomeRoot},
		{"carriage return", "/home/me/a\rb", artifactfetch.CodeInvalidPath, artifactfetch.RuleHomeRoot},
		{"too long", "/home/me/" + strings.Repeat("a", 8200), artifactfetch.CodeInvalidPath, artifactfetch.RuleHomeRoot},

		{"outside home", "/etc/passwd", artifactfetch.CodeOutsideHome, artifactfetch.RuleHomeRoot},
		{"dotdot escape", "/home/me/../other/x.md", artifactfetch.CodeOutsideHome, artifactfetch.RuleHomeRoot},
		{"sibling prefix", "/home/meme/x.md", artifactfetch.CodeOutsideHome, artifactfetch.RuleHomeRoot},

		{"hidden file", "/home/me/proj/.env", artifactfetch.CodeHiddenPathDenied, artifactfetch.RuleHiddenPath},
		{"hidden dir", "/home/me/.ssh/known_hosts", artifactfetch.CodeHiddenPathDenied, artifactfetch.RuleHiddenPath},
		{"hidden after allowlisted", "/home/me/.claude/.credentials.json", artifactfetch.CodeHiddenPathDenied, artifactfetch.RuleHiddenPath},
		{"hidden wins over sensitive", "/home/me/.ssh/id_rsa", artifactfetch.CodeHiddenPathDenied, artifactfetch.RuleHiddenPath},

		{"git config", "/home/me/proj/.git/config", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleCredentialPath},
		{"git config uppercase", "/home/me/proj/.GIT/CONFIG", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleCredentialPath},
		{"codex auth", "/home/me/.codex/auth.json", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleCredentialPath},
		{"codex config", "/home/me/.codex/config.toml", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleCredentialPath},
		{"claude settings", "/home/me/.claude/settings.json", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleCredentialPath},
		{"claude settings local", "/home/me/proj/.claude/settings.local.json", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleCredentialPath},
		{"credential wins over sensitive", "/home/me/.codex/auth.json", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleCredentialPath},

		{"pem", "/home/me/certs/server.pem", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"key uppercase", "/home/me/certs/SERVER.KEY", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"p12", "/home/me/a.p12", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"pfx", "/home/me/a.pfx", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"keystore", "/home/me/a.keystore", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"jks", "/home/me/a.jks", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"kdbx", "/home/me/vault.kdbx", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"ppk", "/home/me/a.ppk", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"sqlite", "/home/me/app/auth.sqlite", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"sqlite3", "/home/me/app/auth.sqlite3", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"db", "/home/me/app/state.db", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"sqlite wal", "/home/me/app/auth.sqlite-wal", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"db shm", "/home/me/app/state.db-shm", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"sqlite3 wal uppercase", "/home/me/app/AUTH.SQLITE3-WAL", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"id_rsa outside ssh dir", "/home/me/backup/id_rsa", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"id_ed25519 pub", "/home/me/backup/id_ed25519.pub", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"id_ecdsa", "/home/me/backup/ID_ECDSA", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
		{"id_dsa", "/home/me/backup/id_dsa", artifactfetch.CodeCredentialPathDenied, artifactfetch.RuleSensitiveFile},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, denial := artifactfetch.CheckPath(home, tc.path)
			if tc.code == "" {
				require.Nil(t, denial, "unexpected denial %+v", denial)
				assert.Equal(t, "/home/me", home)
				assert.True(t, strings.HasPrefix(target.Path, home+"/"))
				return
			}
			require.NotNil(t, denial)
			assert.Equal(t, tc.code, denial.Code)
			assert.Equal(t, tc.rule, denial.Rule)
			assert.NotEmpty(t, denial.Message)
		})
	}
}

func TestCheckPathGivenAllowedPathThenReturnsCleanedTarget(t *testing.T) {
	target, denial := artifactfetch.CheckPath("/home/me", "/home/me/proj/./sub/../notes.md")

	require.Nil(t, denial)
	assert.Equal(t, "/home/me/proj/notes.md", target.Path)
	assert.Equal(t, "proj/notes.md", target.Rel)
	assert.Equal(t, "notes.md", target.Filename)
}
