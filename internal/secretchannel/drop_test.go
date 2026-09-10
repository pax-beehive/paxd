package secretchannel

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newTestFileDrop(t *testing.T, clock *fakeClock) FileDrop {
	t.Helper()
	if clock == nil {
		clock = newFakeClock()
	}
	return FileDrop{
		Dir: filepath.Join(t.TempDir(), "transient"),
		TTL: 10 * time.Minute,
		Now: clock.Now,
	}
}

func TestFileDropWriteCreatesFileWithExactBytes(t *testing.T) {
	drop := newTestFileDrop(t, nil)
	plaintext := []byte("sk-secret-value\x00with-a-nul-and-no-trailing-newline")

	ref, expiresAt, err := drop.Write(plaintext)
	require.NoError(t, err)
	require.True(t, expiresAt.After(drop.Now()))

	path := requireFileRefPath(t, ref)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, plaintext, got, "must preserve raw bytes: no trim, no added newline")

	dirInfo, err := os.Stat(drop.Dir)
	require.NoError(t, err)
	require.True(t, dirInfo.IsDir())
	require.Equal(t, os.FileMode(0700), dirInfo.Mode().Perm())
}

func TestFileDropWriteProducesDistinctFiles(t *testing.T) {
	drop := newTestFileDrop(t, nil)

	refA, _, err := drop.Write([]byte("a"))
	require.NoError(t, err)
	refB, _, err := drop.Write([]byte("b"))
	require.NoError(t, err)

	require.NotEqual(t, refA, refB)
	pathA := requireFileRefPath(t, refA)
	pathB := requireFileRefPath(t, refB)
	require.NotEqual(t, pathA, pathB)

	gotA, err := os.ReadFile(pathA)
	require.NoError(t, err)
	require.Equal(t, []byte("a"), gotA)
	gotB, err := os.ReadFile(pathB)
	require.NoError(t, err)
	require.Equal(t, []byte("b"), gotB)
}

func TestFileDropWriteFailsWhenDirPathIsAFile(t *testing.T) {
	parent := t.TempDir()
	blocked := filepath.Join(parent, "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("not a directory"), 0600))

	drop := FileDrop{Dir: filepath.Join(blocked, "transient"), TTL: time.Minute, Now: time.Now}
	_, _, err := drop.Write([]byte("secret"))
	require.Error(t, err)
}

func TestFileDropSweepRemovesOnlyExpiredFiles(t *testing.T) {
	clock := newFakeClock()
	drop := newTestFileDrop(t, clock)

	oldRef, _, err := drop.Write([]byte("old"))
	require.NoError(t, err)
	clock.Advance(5 * time.Minute)
	newRef, _, err := drop.Write([]byte("new"))
	require.NoError(t, err)

	clock.Advance(6 * time.Minute) // old is now 11m old (> 10m TTL), new is 6m old

	removed, err := drop.Sweep()
	require.NoError(t, err)
	require.Equal(t, 1, removed)

	_, err = os.Stat(requireFileRefPath(t, oldRef))
	require.True(t, os.IsNotExist(err), "expired file must be gone")
	_, err = os.Stat(requireFileRefPath(t, newRef))
	require.NoError(t, err, "unexpired file must survive")
}

func TestFileDropSweepOnEmptyOrMissingDirIsNoop(t *testing.T) {
	drop := newTestFileDrop(t, nil)
	removed, err := drop.Sweep()
	require.NoError(t, err)
	require.Equal(t, 0, removed)
}

func TestFileDropCleanupStartupRemovesEverything(t *testing.T) {
	clock := newFakeClock()
	drop := newTestFileDrop(t, clock)

	_, _, err := drop.Write([]byte("a"))
	require.NoError(t, err)
	// Even a file written "just now" (well within TTL) must not survive a
	// restart: no in-memory channel state can possibly still reference it.
	_, _, err = drop.Write([]byte("b"))
	require.NoError(t, err)

	removed, err := drop.CleanupStartup()
	require.NoError(t, err)
	require.Equal(t, 2, removed)

	entries, err := os.ReadDir(drop.Dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func requireFileRefPath(t *testing.T, ref string) string {
	t.Helper()
	require.True(t, len(ref) > len(fileRefPrefix) && ref[:len(fileRefPrefix)] == fileRefPrefix, "ref must use the file: prefix, got %q", ref)
	return ref[len(fileRefPrefix):]
}
