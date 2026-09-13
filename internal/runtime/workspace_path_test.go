package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveSessionWorkspace(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	require.NoError(t, os.Mkdir(project, 0o700))
	file := filepath.Join(home, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("not a directory"), 0o600))
	t.Setenv("HOME", home)

	tests := []struct {
		name      string
		input     string
		want      string
		errorCode string
	}{
		{
			name:  "given absolute directory",
			input: project,
			want:  project,
		},
		{
			name:  "given home directory",
			input: "~",
			want:  home,
		},
		{
			name:  "given directory below home",
			input: "~/project",
			want:  project,
		},
		{
			name:      "given ordinary relative path",
			input:     "project",
			errorCode: "workspace_path_invalid",
		},
		{
			name:      "given another users home",
			input:     "~alice/project",
			errorCode: "workspace_path_invalid",
		},
		{
			name:  "given missing nested directory below home",
			input: "~/missing/nested/project",
			want:  filepath.Join(home, "missing", "nested", "project"),
		},
		{
			name:  "given missing absolute directory",
			input: filepath.Join(home, "absolute", "project"),
			want:  filepath.Join(home, "absolute", "project"),
		},
		{
			name:      "given file path",
			input:     file,
			errorCode: "workspace_not_directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSessionWorkspace(tt.input, true)

			if tt.errorCode != "" {
				require.Error(t, err)
				assert.Equal(t, tt.errorCode, routerErrorCode(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.True(t, filepath.IsAbs(got))
			assert.DirExists(t, got)
		})
	}
}

func TestResolveSessionWorkspaceDoesNotCreateOnResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "project")
	_, err := resolveSessionWorkspace(path, false)
	require.Error(t, err)
	assert.Equal(t, "workspace_not_found", routerErrorCode(err))
	assert.NoDirExists(t, filepath.Dir(path))
}

func TestResolveSessionWorkspaceRejectsFileAncestor(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("keep"), 0o600))
	_, err := resolveSessionWorkspace(filepath.Join(file, "nested", "project"), true)
	require.Error(t, err)
	content, err := os.ReadFile(file)
	require.NoError(t, err)
	assert.Equal(t, "keep", string(content))
}

func TestResolveSessionWorkspacePermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix permissions and an unprivileged user")
	}
	for _, mode := range []os.FileMode{0o500, 0o000} {
		t.Run(mode.String(), func(t *testing.T) {
			parent := t.TempDir()
			require.NoError(t, os.Chmod(parent, mode))
			t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0o700)) })
			_, err := resolveSessionWorkspace(filepath.Join(parent, "nested", "project"), true)
			require.Error(t, err)
			assert.Equal(t, "workspace_permission_denied", routerErrorCode(err))
			require.NoError(t, os.Chmod(parent, 0o700))
			assert.NoDirExists(t, filepath.Join(parent, "nested"))
		})
	}
}

func TestResolveSessionWorkspaceConcurrentCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "project")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := resolveSessionWorkspace(path, true)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.DirExists(t, path)
}
