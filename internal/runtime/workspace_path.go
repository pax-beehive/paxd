package runtime

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func resolveSessionWorkspace(input string) (string, error) {
	value := strings.TrimSpace(input)
	switch {
	case value == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ACPRouterError{
				Code:    "workspace_home_unavailable",
				Message: "resolve runtime home directory: " + err.Error(),
			}
		}
		value = home
	case strings.HasPrefix(value, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ACPRouterError{
				Code:    "workspace_home_unavailable",
				Message: "resolve runtime home directory: " + err.Error(),
			}
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	case filepath.IsAbs(value):
	default:
		return "", ACPRouterError{
			Code:    "workspace_path_invalid",
			Message: "workspace must be an absolute path, ~, or start with ~/",
		}
	}

	value = filepath.Clean(value)
	if !filepath.IsAbs(value) {
		return "", ACPRouterError{
			Code:    "workspace_path_invalid",
			Message: "resolved workspace must be an absolute path",
		}
	}
	info, err := os.Stat(value)
	if err != nil {
		code := "workspace_unavailable"
		if errors.Is(err, fs.ErrNotExist) {
			code = "workspace_not_found"
		}
		return "", ACPRouterError{
			Code:    code,
			Message: "inspect workspace directory: " + err.Error(),
		}
	}
	if !info.IsDir() {
		return "", ACPRouterError{
			Code:    "workspace_not_directory",
			Message: "workspace path is not a directory",
		}
	}
	return value, nil
}
