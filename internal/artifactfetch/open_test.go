package artifactfetch_test

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactfetch"
)

func TestCheckGivenFetchableFileThenAllowsWithSize(t *testing.T) {
	home := t.TempDir()
	path := writeHomeFile(t, home, "proj/notes.md", "hello world", 0o644)

	result, err := artifactfetch.Check(home, path)

	require.NoError(t, err)
	assert.True(t, result.Allowed)
	assert.Nil(t, result.Denial)
	assert.Equal(t, path, result.Path)
	assert.Equal(t, "notes.md", result.Filename)
	assert.Equal(t, int64(len("hello world")), result.SizeBytes)
}

func TestCheckGivenFileStateProblemsThenReturnsDenials(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	outsideFile := writeHomeFile(t, outside, "secret.md", "outside", 0o644)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "proj", "dir.md"), 0o755))
	require.NoError(t, os.Symlink(outsideFile, filepath.Join(home, "proj", "escape.md")))
	require.NoError(t, os.Symlink(outside, filepath.Join(home, "linkdir")))
	inside := writeHomeFile(t, home, "proj/real.md", "inside", 0o644)
	require.NoError(t, os.Symlink("real.md", filepath.Join(home, "proj", "inside-link.md")))
	// os.Root never follows absolute symlink targets, even ones inside home.
	require.NoError(t, os.Symlink(inside, filepath.Join(home, "proj", "absolute-link.md")))

	cases := []struct {
		name string
		path string
		code string
		rule string
	}{
		{"missing", filepath.Join(home, "proj", "missing.md"), artifactfetch.CodeNotFound, artifactfetch.RuleHomeRoot},
		{"directory", filepath.Join(home, "proj", "dir.md"), artifactfetch.CodeNotRegularFile, artifactfetch.RuleHomeRoot},
		{"symlink file escape", filepath.Join(home, "proj", "escape.md"), artifactfetch.CodeOutsideHome, artifactfetch.RuleHomeRoot},
		{"symlink dir escape", filepath.Join(home, "linkdir", "secret.md"), artifactfetch.CodeOutsideHome, artifactfetch.RuleHomeRoot},
		{"absolute symlink", filepath.Join(home, "proj", "absolute-link.md"), artifactfetch.CodeOutsideHome, artifactfetch.RuleHomeRoot},
		{"path rule before open", filepath.Join(home, "proj", ".env"), artifactfetch.CodeHiddenPathDenied, artifactfetch.RuleHiddenPath},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := artifactfetch.Check(home, tc.path)

			require.NoError(t, err)
			assert.False(t, result.Allowed)
			require.NotNil(t, result.Denial)
			assert.Equal(t, tc.code, result.Denial.Code)
			assert.Equal(t, tc.rule, result.Denial.Rule)
			assert.Equal(t, filepath.Base(tc.path), result.Filename)
		})
	}

	t.Run("symlink inside home is followed", func(t *testing.T) {
		result, err := artifactfetch.Check(home, filepath.Join(home, "proj", "inside-link.md"))
		require.NoError(t, err)
		assert.True(t, result.Allowed, "%+v", result.Denial)
	})
}

func TestCheckGivenOwnerOnlyModesThenDeniesWithChmodHint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only rule is not applied on windows")
	}
	home := t.TempDir()
	for _, mode := range []os.FileMode{0o600, 0o400, 0o700} {
		path := writeHomeFile(t, home, "proj/private-"+mode.String()+".md", "x", mode)

		result, err := artifactfetch.Check(home, path)

		require.NoError(t, err)
		require.NotNil(t, result.Denial, mode.String())
		assert.Equal(t, artifactfetch.CodeOwnerOnlyDenied, result.Denial.Code)
		assert.Equal(t, artifactfetch.RuleOwnerOnly, result.Denial.Rule)
		assert.Contains(t, result.Denial.Message, "chmod 644")
		assert.Contains(t, result.Denial.Message, path)
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		path := writeHomeFile(t, home, "proj/shared-"+mode.String()+".md", "x", mode)
		result, err := artifactfetch.Check(home, path)
		require.NoError(t, err)
		assert.True(t, result.Allowed, mode.String())
	}
}

func TestCheckGivenUnreadableFileThenPermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires unix permission enforcement")
	}
	home := t.TempDir()
	path := writeHomeFile(t, home, "proj/locked.md", "x", 0o044)

	result, err := artifactfetch.Check(home, path)

	require.NoError(t, err)
	require.NotNil(t, result.Denial)
	assert.Equal(t, artifactfetch.CodePermissionDenied, result.Denial.Code)
	assert.Equal(t, artifactfetch.RuleHomeRoot, result.Denial.Rule)
}

func TestCheckInfoGivenOversizedOrIrregularInfoThenDenies(t *testing.T) {
	big := fakeInfo{mode: 0o644, size: artifactfetch.MaxSizeBytes + 1}
	denial := artifactfetch.CheckInfo("/home/me/big.iso", big)
	require.NotNil(t, denial)
	assert.Equal(t, artifactfetch.CodeTooLarge, denial.Code)
	assert.Equal(t, artifactfetch.RuleSizeLimit, denial.Rule)

	exact := fakeInfo{mode: 0o644, size: artifactfetch.MaxSizeBytes}
	assert.Nil(t, artifactfetch.CheckInfo("/home/me/ok.iso", exact))

	device := fakeInfo{mode: fs.ModeDevice | 0o644}
	denial = artifactfetch.CheckInfo("/home/me/dev", device)
	require.NotNil(t, denial)
	assert.Equal(t, artifactfetch.CodeNotRegularFile, denial.Code)
}

func TestOpenGivenFetchableFileThenReturnsReadableHandle(t *testing.T) {
	home := t.TempDir()
	path := writeHomeFile(t, home, "notes.md", "body", 0o644)

	opened, denial, err := artifactfetch.Open(home, path)

	require.NoError(t, err)
	require.Nil(t, denial)
	defer opened.File.Close()
	content, err := io.ReadAll(opened.File)
	require.NoError(t, err)
	assert.Equal(t, "body", string(content))
	assert.Equal(t, int64(4), opened.Size)
	assert.Equal(t, "notes.md", opened.Filename)
}

func writeHomeFile(t *testing.T, home, rel, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(home, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	return path
}

type fakeInfo struct {
	mode fs.FileMode
	size int64
}

func (f fakeInfo) Name() string       { return "fake" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

// A relative symlink stays inside the home root, so os.Root follows it. The
// path rules must judge the file it resolves to, not the link name, or a
// link such as proj/notes.md -> ../.env would leak a hidden file.
func TestCheckGivenSymlinkToProtectedFileThenJudgesResolvedTarget(t *testing.T) {
	home := t.TempDir()
	writeHomeFile(t, home, ".env", "TOKEN=x", 0o644)
	writeHomeFile(t, home, "proj/.git/config", "[remote]", 0o644)
	writeHomeFile(t, home, "proj/keys/server.pem", "pem", 0o644)
	writeHomeFile(t, home, "proj/real.md", "inside", 0o644)
	links := map[string]string{
		"proj/env.md":    "../.env",
		"proj/git.md":    ".git/config",
		"proj/pem.md":    "keys/server.pem",
		"proj/nested.md": "env.md",
		"proj/ok.md":     "real.md",
	}
	for link, target := range links {
		require.NoError(t, os.Symlink(target, filepath.Join(home, link)))
	}

	cases := map[string]string{
		"proj/env.md":    artifactfetch.CodeHiddenPathDenied,
		"proj/git.md":    artifactfetch.CodeCredentialPathDenied,
		"proj/pem.md":    artifactfetch.CodeCredentialPathDenied,
		"proj/nested.md": artifactfetch.CodeHiddenPathDenied,
	}
	for link, code := range cases {
		t.Run(link, func(t *testing.T) {
			result, err := artifactfetch.Check(home, filepath.Join(home, link))
			require.NoError(t, err)
			assert.False(t, result.Allowed)
			require.NotNil(t, result.Denial)
			assert.Equal(t, code, result.Denial.Code)
		})
	}

	result, err := artifactfetch.Check(home, filepath.Join(home, "proj/ok.md"))
	require.NoError(t, err)
	assert.True(t, result.Allowed, "a link to an ordinary file stays fetchable")
}

func TestOpenGivenSymlinkToProtectedFileThenDenies(t *testing.T) {
	home := t.TempDir()
	writeHomeFile(t, home, ".env", "TOKEN=x", 0o644)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "proj"), 0o755))
	require.NoError(t, os.Symlink("../.env", filepath.Join(home, "proj", "env.md")))

	opened, denial, err := artifactfetch.Open(home, filepath.Join(home, "proj", "env.md"))

	require.NoError(t, err)
	assert.Nil(t, opened)
	require.NotNil(t, denial)
	assert.Equal(t, artifactfetch.CodeHiddenPathDenied, denial.Code)
}
