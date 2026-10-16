package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHomeDirectoryBoundaryAndLazyPages(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, "project"), 0700))
	require.NoError(t, os.Mkdir(filepath.Join(home, "project", "nested"), 0700))
	require.NoError(t, os.Mkdir(filepath.Join(home, ".hidden"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(home, "file.txt"), []byte("private contents"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(home, "escape")))
	require.NoError(t, os.Symlink("project", filepath.Join(home, "inside")))
	page, err := listHomeDirectories(ctx, home, ListDirectoriesQuery{})
	require.NoError(t, err)
	require.Empty(t, page.Parent)
	require.Equal(t, []DirectoryEntry{{"inside", filepath.Join(home, "inside")}, {"project", filepath.Join(home, "project")}}, page.Entries)
	for _, path := range []string{outside, filepath.Join(home, ".."), filepath.Join(home, "escape"), filepath.Join(home, "escape", "sub")} {
		_, err := listHomeDirectories(ctx, home, ListDirectoriesQuery{Path: path})
		require.Error(t, err, path)
	}
	page, err = listHomeDirectories(ctx, home, ListDirectoriesQuery{Path: filepath.Join(home, "inside")})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	require.Equal(t, "nested", page.Entries[0].Name)
	require.Equal(t, home, page.Parent)
	page, err = listHomeDirectories(ctx, home, ListDirectoriesQuery{ShowHidden: true})
	require.NoError(t, err)
	require.Len(t, page.Entries, 3)
	for i := 0; i < 205; i++ {
		require.NoError(t, os.Mkdir(filepath.Join(home, fmt.Sprintf("d%03d", i)), 0700))
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err = listHomeDirectories(ctx, home, ListDirectoriesQuery{After: cursor})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Entries), 100)
		for _, e := range page.Entries {
			require.False(t, seen[e.Name])
			seen[e.Name] = true
		}
		if page.NextAfter == "" {
			break
		}
		cursor = page.NextAfter
	}
	require.Len(t, seen, 207)
}

func TestDirectoryQueryValidationAndAuthentication(t *testing.T) {
	for _, q := range []ListDirectoriesQuery{{Path: "../"}, {Path: "relative"}, {Path: "/a\x00"}, {After: "../x"}} {
		require.Error(t, q.Validate())
	}
	q := Query{Type: QueryDirectoriesList, ListDirectories: &ListDirectoriesQuery{}}
	require.NoError(t, q.Validate())
	result, err := (&ControlService{}).HandleQuery(context.Background(), Source{Kind: SourceRemote}, q)
	require.NoError(t, err)
	require.Equal(t, "unauthorized", result.Error.Code)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	home := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(home, "child"), 0700))
	_, err = listHomeDirectories(ctx, home, ListDirectoriesQuery{})
	require.ErrorIs(t, err, context.Canceled)
}
