package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestServiceRunArgsIncludesConfiguredRunFlags(t *testing.T) {
	args := serviceRunArgs("/usr/local/bin/paxd", serviceInstallOptions{
		ControlSocket: "none",
		DebugHTTP:     "127.0.0.1:8765",
	})

	assert.Equal(t, []string{
		"/usr/local/bin/paxd",
		"run",
		"--control-socket",
		"none",
		"--debug-http",
		"127.0.0.1:8765",
	}, args)
}

func TestServiceCommandUsesInstallUninstallVocabulary(t *testing.T) {
	cmd := cmdServiceCommand()

	names := make([]string, 0, len(cmd.Commands))
	for _, command := range cmd.Commands {
		names = append(names, command.Name)
	}

	assert.ElementsMatch(t, []string{"install", "start", "stop", "restart", "status", "uninstall"}, names)
	assert.NotContains(t, names, "enable-auto-start")
	assert.NotContains(t, names, "disable-auto-start")
}

func TestServiceInstallDoesNotExposeConfigFlag(t *testing.T) {
	cmd := cmdServiceCommand()
	var install *cli.Command
	for _, command := range cmd.Commands {
		if command.Name == "install" {
			install = command
			break
		}
	}
	require.NotNil(t, install)

	names := make([]string, 0, len(install.Flags))
	for _, flag := range install.Flags {
		names = append(names, flag.Names()...)
	}

	assert.Contains(t, names, "force")
	assert.Contains(t, names, "control-socket")
	assert.Contains(t, names, "debug-http")
	assert.NotContains(t, names, "config")
}

func TestGenerateLaunchdPlistRunsPaxdInForegroundUnderLaunchd(t *testing.T) {
	target := serviceTarget{
		GOOS:     "darwin",
		ExecPath: "/Applications/Pax/paxd",
		Home:     "/Users/dev",
	}

	plist := generateLaunchdPlist(target, serviceInstallOptions{
		DebugHTTP: "127.0.0.1:8765",
	})

	assert.Contains(t, plist, "<string>com.paxtech.paxd</string>")
	assert.NotContains(t, plist, "com.toddzheng.paxd")
	assert.Contains(t, plist, "<key>RunAtLoad</key>")
	assert.Contains(t, plist, "<key>KeepAlive</key>")
	assert.Contains(t, plist, "<string>/Applications/Pax/paxd</string>")
	assert.Contains(t, plist, "<string>run</string>")
	assert.Contains(t, plist, "<key>EnvironmentVariables</key>")
	assert.Contains(t, plist, "/Users/dev/.local/bin")
	assert.Contains(t, plist, "/Users/dev/bin")
	assert.Contains(t, plist, "/opt/homebrew/bin")
	assert.Contains(t, plist, "/Users/dev/.local/share/fnm/aliases/default/bin")
	assert.Contains(t, plist, "<string>--debug-http</string>")
	assert.Contains(t, plist, "<string>127.0.0.1:8765</string>")
	assert.NotContains(t, plist, "StandardOutPath")
	assert.Contains(t, plist, "<string>/Users/dev/.paxd/logs/paxd.error.log</string>")
}

func TestGenerateLaunchdPlistEscapesProgramArguments(t *testing.T) {
	target := serviceTarget{
		GOOS:     "darwin",
		ExecPath: "/tmp/Pax & Tools/paxd",
		Home:     "/Users/dev",
	}

	plist := generateLaunchdPlist(target, serviceInstallOptions{DebugHTTP: "127.0.0.1:8765"})

	assert.Contains(t, plist, "<string>/tmp/Pax &amp; Tools/paxd</string>")
}

func TestGenerateSystemdUserUnitRunsPaxdInForeground(t *testing.T) {
	target := serviceTarget{
		GOOS:     "linux",
		ExecPath: "/home/dev/bin/paxd",
		Home:     "/home/dev",
		EnvPATH:  "/home/dev/.local/bin:/custom/bin:/usr/bin",
	}

	unit := generateSystemdUnit(target, serviceInstallOptions{
		ControlSocket: "none",
	})

	assert.Contains(t, unit, "Description=Pax Fleet Daemon")
	assert.Contains(t, unit, `ExecStart=/home/dev/bin/paxd run --control-socket none`)
	assert.Contains(t, unit, `WorkingDirectory=/home/dev`)
	assert.Contains(t, unit, `Environment=HOME=/home/dev`)
	assert.Contains(t, unit, `Environment=PATH=/home/dev/.local/bin:/custom/bin:/usr/bin`)
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
	assert.Contains(t, unit, `ExecStart=/usr/local/bin/paxd run`)
	assert.Contains(t, unit, "WantedBy=multi-user.target")
}

func TestGenerateSystemdUnitEscapesPathsWithoutQuotingThem(t *testing.T) {
	target := serviceTarget{
		GOOS:     "linux",
		ExecPath: "/home/dev/Pax Tools/paxd",
		Home:     "/home/dev/Pax Home",
		EnvPATH:  "/home/dev/Pax Home/.local/bin:/usr/bin",
	}

	unit := generateSystemdUnit(target, serviceInstallOptions{
		DebugHTTP: "127.0.0.1:%8765",
	})

	assert.Contains(t, unit, `ExecStart=/home/dev/Pax\sTools/paxd run --debug-http 127.0.0.1:%%8765`)
	assert.Contains(t, unit, `WorkingDirectory=/home/dev/Pax\sHome`)
	assert.Contains(t, unit, `Environment=HOME=/home/dev/Pax\sHome`)
	assert.Contains(t, unit, `Environment=PATH=/home/dev/Pax\sHome/.local/bin`)
	assert.NotContains(t, unit, `WorkingDirectory="/home/dev/Pax Home"`)
}

func TestGenerateSystemdUnitFallsBackToDefaultPathWhenInstallPathIsEmpty(t *testing.T) {
	target := serviceTarget{
		GOOS:     "linux",
		ExecPath: "/home/dev/bin/paxd",
		Home:     "/home/dev",
	}

	unit := generateSystemdUnit(target, serviceInstallOptions{})

	assert.Contains(t, unit, `Environment=PATH=/home/dev/.local/bin`)
	assert.Contains(t, unit, `/home/dev/bin`)
}

func TestServicePathsAndSystemctlArgs(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "dev")

	assert.Equal(t, filepath.Join(home, "Library", "LaunchAgents", "com.paxtech.paxd.plist"), launchdPlistPath(home))
	assert.Equal(t, filepath.Join(home, "Library", "LaunchAgents", "com.toddzheng.paxd.plist"), launchdPlistPathForLabel(home, "com.toddzheng.paxd"))
	assert.Equal(t, filepath.Join(home, ".config", "systemd", "user", "paxd.service"), systemdUnitPath(home, false))
	assert.Equal(t, filepath.Join(string(filepath.Separator), "etc", "systemd", "system", "paxd.service"), systemdUnitPath(home, true))
	assert.Equal(t, filepath.Join(home, ".paxd", "logs"), serviceLogDir(home))
	assert.Contains(t, servicePATH(home), filepath.Join(home, ".local", "bin"))
	assert.Contains(t, servicePATH(home), filepath.Join(home, ".local", "share", "fnm", "aliases", "default", "bin"))
	assert.Equal(t, []string{"systemctl", "--user", "start", "paxd"}, systemctlArgs("start", false, "paxd"))
	assert.Equal(t, []string{"systemctl", "start", "paxd"}, systemctlArgs("start", true, "paxd"))
}

func TestCleanupLegacyLaunchdServicesRemovesOldPlist(t *testing.T) {
	home := t.TempDir()
	legacyPath := launchdPlistPathForLabel(home, "com.toddzheng.paxd")
	require.NoError(t, os.MkdirAll(filepath.Dir(legacyPath), 0755))
	require.NoError(t, os.WriteFile(legacyPath, []byte("legacy"), 0644))
	original := runCommand
	originalQuiet := runCommandQuiet
	var calls []string
	runCommandQuiet = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	defer func() {
		runCommand = original
		runCommandQuiet = originalQuiet
	}()

	err := cleanupLegacyLaunchdServices(serviceTarget{Home: home})

	require.NoError(t, err)
	assert.NoFileExists(t, legacyPath)
	assert.Contains(t, calls, "launchctl bootout "+launchdDomain()+"/com.toddzheng.paxd")
}

func TestLaunchdStartBootsOutQuietlyBeforeBootstrap(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Dir(launchdPlistPath(home)), 0755))
	require.NoError(t, os.WriteFile(launchdPlistPath(home), []byte("plist"), 0644))
	original := runCommand
	originalQuiet := runCommandQuiet
	var calls []string
	runCommand = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	runCommandQuiet = func(name string, args ...string) error {
		calls = append(calls, "quiet "+name+" "+strings.Join(args, " "))
		return nil
	}
	defer func() {
		runCommand = original
		runCommandQuiet = originalQuiet
	}()

	err := launchdControl(serviceTarget{Home: home}, "start")

	require.NoError(t, err)
	assert.Equal(t, []string{
		"quiet launchctl bootout " + launchdDomain() + "/com.paxtech.paxd",
		"quiet launchctl bootstrap " + launchdDomain() + " " + launchdPlistPath(home),
	}, calls)
}

func TestLaunchdBootstrapRetriesTransientFailure(t *testing.T) {
	original := runCommand
	originalQuiet := runCommandQuiet
	quietCalls := 0
	runCommandQuiet = func(name string, args ...string) error {
		quietCalls++
		if quietCalls == 1 {
			return errors.New("launchd still unloading")
		}
		return nil
	}
	runCommand = func(name string, args ...string) error {
		t.Fatalf("runCommand should not be called after retry succeeds")
		return nil
	}
	defer func() {
		runCommand = original
		runCommandQuiet = originalQuiet
	}()

	err := launchdBootstrap("/tmp/com.paxtech.paxd.plist")

	require.NoError(t, err)
	assert.Equal(t, 2, quietCalls)
}

func TestJoinSystemdArgsQuotesSpaces(t *testing.T) {
	got := joinSystemdArgs([]string{"/tmp/Pax Tools/paxd", "run", "--debug-http", "127.0.0.1:8765"})

	require.True(t, strings.Contains(got, `/tmp/Pax\sTools/paxd`))
	assert.Contains(t, got, `127.0.0.1:8765`)
	assert.NotContains(t, got, `"`)
}

func TestJoinSystemdArgsEscapesExecSeparatorsAndSpecifiers(t *testing.T) {
	got := joinSystemdArgs([]string{"/tmp/paxd", "run", "--debug-http", "127.0.0.1:%8765", "semi;colon"})

	assert.Contains(t, got, `127.0.0.1:%%8765`)
	assert.Contains(t, got, `semi\;colon`)
}
