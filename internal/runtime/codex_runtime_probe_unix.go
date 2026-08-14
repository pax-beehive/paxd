//go:build !windows

package runtime

import "os"

func codexProbeExecutableNames(map[string]string) []string {
	return []string{"codex"}
}

func environmentOverride(env map[string]string, key string) (string, bool) {
	value, ok := env[key]
	return value, ok
}

func codexProbeFileIsExecutable(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}
