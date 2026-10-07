// Package paxlinstall manages the paxl executable used by this daemon.
package paxlinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Observation struct {
	Status    string    `json:"status"`
	Version   string    `json:"version,omitempty"`
	Commit    string    `json:"commit,omitempty"`
	Path      string    `json:"path,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Error     string    `json:"error,omitempty"`
}

// ResolveCommand is shared with session discovery so reporting and execution agree.
func ResolveCommand(command []string) ([]string, error) {
	if len(command) == 0 {
		command = strings.Fields(os.Getenv("PAXD_PAXL_COMMAND"))
	}
	if len(command) > 0 {
		path, err := exec.LookPath(command[0])
		if err != nil {
			return nil, err
		}
		return append([]string{path}, command[1:]...), nil
	}
	path, err := exec.LookPath("paxl")
	if err == nil {
		return []string{path}, nil
	}
	executable, exErr := os.Executable()
	if exErr == nil {
		candidate := filepath.Join(filepath.Dir(executable), "paxl")
		if path, e := exec.LookPath(candidate); e == nil {
			return []string{path}, nil
		}
	}
	return nil, err
}

func Probe(ctx context.Context) Observation {
	observation := Observation{Status: "probe_failed", CheckedAt: time.Now().UTC()}
	command, err := ResolveCommand(nil)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			observation.Status = "missing"
		}
		observation.Error = err.Error()
		return observation
	}
	observation.Path, err = filepath.EvalSymlinks(command[0])
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	observation.Path, err = filepath.Abs(observation.Path)
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	if len(command) != 1 {
		observation.Error = "paxl command wrappers cannot be managed remotely"
		return observation
	}
	metadata, err := versionAt(ctx, observation.Path)
	if err != nil {
		observation.Error = err.Error()
		return observation
	}
	observation.Version, observation.Commit = metadata.Version, metadata.Commit
	observation.Status = "installed"
	return observation
}

func versionAt(parent context.Context, path string) (Observation, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version", "--format", "json") // #nosec G204 -- locally resolved executable.
	cmd.WaitDelay = time.Second
	output := &limitedOutput{}
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return Observation{}, fmt.Errorf("probe paxl version: %w", err)
	}
	var result Observation
	if err := json.Unmarshal(output.data, &result); err != nil {
		return result, fmt.Errorf("decode paxl version: %w", err)
	}
	if strings.TrimSpace(result.Version) == "" {
		return result, errors.New("paxl returned an empty version")
	}
	return result, nil
}

type limitedOutput struct{ data []byte }

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(w.data)+len(p) > 16384 {
		return 0, errors.New("paxl version output exceeds limit")
	}
	w.data = append(w.data, p...)
	return len(p), nil
}
