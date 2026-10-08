package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func resolveACPExecutable(spec LocalACPProcessSpec, executable string) (string, error) {
	if strings.ContainsAny(executable, `/\`) {
		candidate := executable
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(spec.WorkingDir, candidate)
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			return "", exec.ErrNotFound
		}
		info, err := os.Stat(absolute)
		if err != nil || !codexProbeFileIsExecutable(info) {
			return "", exec.ErrNotFound
		}
		return absolute, nil
	}

	pathValue, overridden := environmentOverride(spec.Env, "PATH")
	if !overridden {
		pathValue = os.Getenv("PATH")
	}
	if pathValue == "" {
		return "", exec.ErrNotFound
	}
	workingDir := strings.TrimSpace(spec.WorkingDir)
	if workingDir == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return "", exec.ErrNotFound
		}
	}
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return "", exec.ErrNotFound
	}
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = absWorkingDir
		} else if !filepath.IsAbs(directory) {
			directory = filepath.Join(absWorkingDir, directory)
		}
		for _, name := range runtimeProbeExecutableNames(spec.Env, executable) {
			candidate := filepath.Join(directory, name)
			info, statErr := os.Stat(candidate)
			if statErr != nil || !codexProbeFileIsExecutable(info) {
				continue
			}
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}
