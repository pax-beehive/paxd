package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceRunArgsIncludesConfiguredRunFlags(t *testing.T) {
	args := serviceRunArgs("/usr/local/bin/paxd", serviceInstallOptions{
		Config:        "/etc/paxd.yaml",
		ControlSocket: "none",
		DebugHTTP:     "127.0.0.1:8765",
	})

	assert.Equal(t, []string{
		"/usr/local/bin/paxd",
		"run",
		"--config",
		"/etc/paxd.yaml",
		"--control-socket",
		"none",
		"--debug-http",
		"127.0.0.1:8765",
	}, args)
}

func TestGenerateLaunchdPlistRunsPaxdInForegroundUnderLaunchd(t *testing.T) {
	target := serviceTarget{
		GOOS:     "darwin",
		ExecPath: "/Applications/Pax/paxd",
		Home:     "/Users/dev",
	}

	plist := generateLaunchdPlist(target, serviceInstallOptions{
		Config:    "/Users/dev/.paxd/paxd.yaml",
		DebugHTTP: "127.0.0.1:8765",
	})

	assert.Contains(t, plist, "<string>com.toddzheng.paxd</string>")
	assert.Contains(t, plist, "<key>RunAtLoad</key>")
	assert.Contains(t, plist, "<key>KeepAlive</key>")
	assert.Contains(t, plist, "<string>/Applications/Pax/paxd</string>")
	assert.Contains(t, plist, "<string>run</string>")
	assert.Contains(t, plist, "<string>--config</string>")
	assert.Contains(t, plist, "<string>/Users/dev/.paxd/paxd.yaml</string>")
	assert.Contains(t, plist, "<string>--debug-http</string>")
	assert.Contains(t, plist, "<string>127.0.0.1:8765</string>")
	assert.Contains(t, plist, "<string>/Users/dev/.paxd/logs/paxd.log</string>")
	assert.Contains(t, plist, "<string>/Users/dev/.paxd/logs/paxd.error.log</string>")
}

func TestGenerateLaunchdPlistEscapesProgramArguments(t *testing.T) {
	target := serviceTarget{
		GOOS:     "darwin",
		ExecPath: "/tmp/Pax & Tools/paxd",
		Home:     "/Users/dev",
	}

	plist := generateLaunchdPlist(target, serviceInstallOptions{Config: "/tmp/a&b.yaml"})

	assert.Contains(t, plist, "<string>/tmp/Pax &amp; Tools/paxd</string>")
	assert.Contains(t, plist, "<string>/tmp/a&amp;b.yaml</string>")
}

func TestGenerateSystemdUserUnitRunsPaxdInForeground(t *testing.T) {
	target := serviceTarget{
		GOOS:     "linux",
		ExecPath: "/home/dev/bin/paxd",
		Home:     "/home/dev",
	}

	unit := generateSystemdUnit(target, serviceInstallOptions{
		Config:        "/home/dev/.paxd/paxd.yaml",
		ControlSocket: "none",
	})

	assert.Contains(t, unit, "Description=Pax Fleet Daemon")
	assert.Contains(t, unit, `ExecStart="/home/dev/bin/paxd" "run" "--config" "/home/dev/.paxd/paxd.yaml" "--control-socket" "none"`)
	assert.Contains(t, unit, `WorkingDirectory="/home/dev"`)
	assert.Contains(t, unit, "Restart=always")
	assert.Contains(t, unit, "RestartSec=5")
	assert.Contains(t, unit, "KillMode=mixed")
	assert.Contains(t, unit, "WantedBy=default.target")
	assert.NotContains(t, unit, "\nUser=")
}

func TestGenerateSystemdSystemUnitRunsAsConfiguredUser(t *testing.T) {
	target := serviceTarget{
		GOOS:     "linux",
		ExecPath: "/usr/local/bin/paxd",
		Home:     "/root",
	}

	unit := generateSystemdUnit(target, serviceInstallOptions{
		System:    true,
		RunAsUser: "pax",
	})

	assert.Contains(t, unit, "User=pax")
	assert.Contains(t, unit, `ExecStart="/usr/local/bin/paxd" "run"`)
	assert.Contains(t, unit, "WantedBy=multi-user.target")
}

func TestServicePathsAndSystemctlArgs(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "dev")

	assert.Equal(t, filepath.Join(home, "Library", "LaunchAgents", "com.toddzheng.paxd.plist"), launchdPlistPath(home))
	assert.Equal(t, filepath.Join(home, ".config", "systemd", "user", "paxd.service"), systemdUnitPath(home, false))
	assert.Equal(t, filepath.Join(string(filepath.Separator), "etc", "systemd", "system", "paxd.service"), systemdUnitPath(home, true))
	assert.Equal(t, filepath.Join(home, ".paxd", "logs"), serviceLogDir(home))
	assert.Equal(t, []string{"systemctl", "--user", "start", "paxd"}, systemctlArgs("start", false, "paxd"))
	assert.Equal(t, []string{"systemctl", "start", "paxd"}, systemctlArgs("start", true, "paxd"))
}

func TestJoinSystemdArgsQuotesSpaces(t *testing.T) {
	got := joinSystemdArgs([]string{"/tmp/Pax Tools/paxd", "run", "--config", "/tmp/a b.yaml"})

	require.True(t, strings.Contains(got, `"/tmp/Pax Tools/paxd"`))
	assert.Contains(t, got, `"/tmp/a b.yaml"`)
}
