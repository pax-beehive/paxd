package artifactfetch_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactfetch"
)

// Fake tokens are assembled at runtime so this source file never contains a
// literal that a repository secret scanner would flag.
func fakeTokens() map[string]string {
	alnum := func(n int) string { return strings.Repeat("Ab3", n/3+1)[:n] }
	upper := func(n int) string { return strings.Repeat("Q7", n/2+1)[:n] }
	hex := func(n int) string { return strings.Repeat("0a9f", n/4+1)[:n] }
	return map[string]string{
		"private-key":            "-----" + "BEGIN OPENSSH " + "PRIVATE KEY" + "-----",
		"private-key/pgp":        "-----" + "BEGIN PGP " + "PRIVATE KEY BLOCK" + "-----",
		"private-key/plain":      "-----" + "BEGIN " + "PRIVATE KEY" + "-----",
		"aws-access-key-id":      "AK" + "IA" + upper(16),
		"aws-access-key-id/temp": "AS" + "IA" + upper(16),
		"github-token":           "gh" + "p_" + alnum(36),
		"github-token/oauth":     "gh" + "o_" + alnum(40),
		"github-token/pat":       "github" + "_pat_" + alnum(30) + "_" + alnum(40),
		"slack-token":            "xo" + "xb-" + "1234567890-" + alnum(24),
		"anthropic-api-key":      "sk-" + "ant-" + "api03-" + alnum(40),
		"openai-api-key":         "sk-" + "proj-" + alnum(48),
		"openai-api-key/legacy":  "sk-" + alnum(48),
		"google-api-key":         "AI" + "za" + alnum(35),
		"stripe-secret-key":      "sk" + "_live_" + alnum(24),
		"stripe-secret-key/rk":   "rk" + "_live_" + alnum(30),
		"pax-api-key":            "pa" + "x_" + hex(48),
		"pax-api-key/user":       "pa" + "xu_" + hex(48),
	}
}

func TestScanGivenFakeSecretThenReportsRuleAndLineWithoutSecret(t *testing.T) {
	for name, token := range fakeTokens() {
		wantRule := strings.SplitN(name, "/", 2)[0]
		t.Run(name, func(t *testing.T) {
			content := "# notes\n\nsome text\nconfig = \"" + token + "\"\nmore\n"
			path := filepath.Join(t.TempDir(), "doc.md")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			finding, err := artifactfetch.ScanFile(path)

			require.NoError(t, err)
			require.NotNil(t, finding, "token %q not detected", name)
			assert.Equal(t, wantRule, finding.Rule)
			assert.Equal(t, 4, finding.Line)
			assert.Equal(t,
				"possible secret detected (rule "+wantRule+", line 4)",
				finding.Message())
			assert.NotContains(t, finding.Message(), token)
		})
	}
}

func TestScanGivenOrdinaryMarkdownThenNoFinding(t *testing.T) {
	content := strings.Join([]string{
		"# Release notes",
		"",
		"Rotate your AWS access key (it starts with AKIA) and never commit an",
		"sk- prefixed API key. Use `ghp_` tokens only via the keychain.",
		"A PRIVATE KEY header looks like BEGIN ... PRIVATE KEY, see docs.",
		"The pax_ node key is 48 hex characters; paxu_ keys belong to users.",
		"Contact: ops@example.com. Hash: 3f2a9c0d.",
		"",
	}, "\n")

	finding, err := artifactfetch.Scanner{}.Scan(strings.NewReader(content))

	require.NoError(t, err)
	assert.Nil(t, finding)
}

func TestScanGivenBinaryFileThenSkipsContent(t *testing.T) {
	var content bytes.Buffer
	content.WriteString("PNG\x00\x01\x02 header")
	content.WriteString("\n" + fakeTokens()["aws-access-key-id"] + "\n")

	finding, err := artifactfetch.Scanner{}.Scan(&content)

	require.NoError(t, err)
	assert.Nil(t, finding)
}

func TestScanGivenNULAfterFirst8KiBThenStillScansAsText(t *testing.T) {
	content := strings.Repeat("a", 9000) + "\x00\n" + fakeTokens()["github-token"] + "\n"

	finding, err := artifactfetch.Scanner{}.Scan(strings.NewReader(content))

	require.NoError(t, err)
	require.NotNil(t, finding)
	assert.Equal(t, "github-token", finding.Rule)
	assert.Equal(t, 2, finding.Line)
}

func TestScanGivenTokenSplitAcrossChunkBoundaryThenStillDetected(t *testing.T) {
	token := fakeTokens()["aws-access-key-id"]
	for split := 1; split < len(token); split++ {
		prefix := strings.Repeat("line\n", 3) + strings.Repeat("x", 64-15-split) + " "
		content := prefix + token + " tail\nafter\n"
		scanner := artifactfetch.Scanner{WindowBytes: 64, OverlapBytes: 32}

		finding, err := scanner.Scan(strings.NewReader(content))

		require.NoError(t, err)
		require.NotNil(t, finding, "split at %d", split)
		assert.Equal(t, "aws-access-key-id", finding.Rule)
		assert.Equal(t, 4, finding.Line, "split at %d", split)
	}
}

func TestScanGivenLongTextAcrossManyWindowsThenTracksLines(t *testing.T) {
	var content strings.Builder
	for i := 0; i < 500; i++ {
		content.WriteString("an ordinary line of markdown text\n")
	}
	content.WriteString("key: " + fakeTokens()["slack-token"] + "\n")
	scanner := artifactfetch.Scanner{WindowBytes: 100, OverlapBytes: 40}

	finding, err := scanner.Scan(strings.NewReader(content.String()))

	require.NoError(t, err)
	require.NotNil(t, finding)
	assert.Equal(t, "slack-token", finding.Rule)
	assert.Equal(t, 501, finding.Line)
}

func TestScanGivenNearMissTokensThenNoFinding(t *testing.T) {
	tokens := fakeTokens()
	content := strings.Join([]string{
		// Embedded in a longer word: the leading \b must not match.
		"x" + tokens["aws-access-key-id"] + "Z",
		// Too short.
		"gh" + "p_" + strings.Repeat("a", 10),
		"pa" + "x_" + strings.Repeat("0", 20),
		"",
	}, "\n")

	finding, err := artifactfetch.Scanner{}.Scan(strings.NewReader(content))

	require.NoError(t, err)
	assert.Nil(t, finding)
}
