package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const piSDKPackage = "@earendil-works/pi-coding-agent"

// ResolvePiSDKBinding returns the SDK package root and the launcher followed on
// restart. An explicit SDK root is independent of CLI launcher upgrades.
func ResolvePiSDKBinding(spec LocalACPProcessSpec) (root, launcher string, err error) {
	configured, set := environmentOverride(spec.Env, "PI_ACP_SDK_ROOT")
	if !set {
		configured, set = os.LookupEnv("PI_ACP_SDK_ROOT")
	}
	if set {
		if !filepath.IsAbs(configured) {
			return "", "", fmt.Errorf("PI_ACP_SDK_ROOT must be an absolute Pi SDK package directory")
		}
		root, err = filepath.EvalSymlinks(configured)
		if err != nil {
			return "", "", fmt.Errorf("resolve PI_ACP_SDK_ROOT: %w", err)
		}
		_, err = readPiSDKManifest(root)
		return root, "", err
	}
	launcher, err = resolveACPExecutable(spec, "pi")
	if err != nil {
		return "", "", fmt.Errorf("resolve Pi SDK: install pi or set PI_ACP_SDK_ROOT: %w", err)
	}
	entry, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return "", "", fmt.Errorf("resolve Pi launcher: %w", err)
	}
	for root = filepath.Dir(entry); ; root = filepath.Dir(root) {
		_, statErr := os.Stat(filepath.Join(root, "package.json"))
		if statErr == nil {
			manifest, readErr := readPiSDKManifest(root)
			if readErr != nil {
				return "", "", readErr
			}
			var bins map[string]string
			var bin string
			if json.Unmarshal(manifest.Bin, &bin) != nil {
				if err := json.Unmarshal(manifest.Bin, &bins); err != nil {
					return "", "", fmt.Errorf("invalid Pi package bin: %w", err)
				}
				bin = bins["pi"]
			}
			target, err := filepath.EvalSymlinks(filepath.Join(root, bin))
			if err != nil || bin == "" || target != entry {
				return "", "", fmt.Errorf("Pi launcher does not match the SDK package bin; set PI_ACP_SDK_ROOT explicitly")
			}
			return root, launcher, nil
		}
		if !os.IsNotExist(statErr) {
			return "", "", fmt.Errorf("read Pi package: %w", statErr)
		}
		if filepath.Dir(root) == root {
			return "", "", fmt.Errorf("Pi launcher is not an installed SDK package; set PI_ACP_SDK_ROOT explicitly")
		}
	}
}

type piSDKManifest struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Bin     json.RawMessage `json:"bin"`
}

func readPiSDKManifest(root string) (*piSDKManifest, error) {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read Pi SDK package.json: %w", err)
	}
	var manifest piSDKManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse Pi SDK package.json: %w", err)
	}
	if manifest.Name != piSDKPackage || manifest.Version == "" {
		return nil, fmt.Errorf("Pi SDK root must contain %s with a version", piSDKPackage)
	}
	return &manifest, nil
}
