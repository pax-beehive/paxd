package runtime

import (
	"os"
	"path/filepath"
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
			name:      "given missing directory",
			input:     "~/missing",
			errorCode: "workspace_not_found",
		},
		{
			name:      "given file path",
			input:     file,
			errorCode: "workspace_not_directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveSessionWorkspace(tt.input)

			if tt.errorCode != "" {
				require.Error(t, err)
				assert.Equal(t, tt.errorCode, routerErrorCode(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.True(t, filepath.IsAbs(got))
		})
	}
}
