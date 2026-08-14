//go:build windows

package runtime

import (
	"os"
	"path/filepath"
	"strings"
)

func codexProbeExecutableNames(env map[string]string) []string {
	pathExt, overridden := environmentOverride(env, "PATHEXT")
	if !overridden {
		pathExt = os.Getenv("PATHEXT")
	}
	if strings.TrimSpace(pathExt) == "" {
		pathExt = ".COM;.EXE;.BAT;.CMD"
	}
	names := []string{"codex"}
	for _, extension := range filepath.SplitList(pathExt) {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			continue
		}
		if !strings.HasPrefix(extension, ".") {
			extension = "." + extension
		}
		names = append(names, "codex"+strings.ToLower(extension), "codex"+strings.ToUpper(extension))
	}
	return names
}

func environmentOverride(env map[string]string, key string) (string, bool) {
	if value, ok := env[key]; ok {
		return value, true
	}
	for candidate, value := range env {
		if strings.EqualFold(candidate, key) {
			return value, true
		}
	}
	return "", false
}

func codexProbeFileIsExecutable(info os.FileInfo) bool {
	return info.Mode().IsRegular()
}
