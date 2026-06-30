package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/remotelogin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestAppExposesDaemonLoginSetupAndServiceCommands(t *testing.T) {
	app := newApp()

	names := make([]string, 0, len(app.Commands))
	for _, command := range app.Commands {
		names = append(names, command.Name)
	}

	assert.ElementsMatch(t, []string{"setup", "login", "update", "run", "service"}, names)
	assert.NotContains(t, names, "connect")
	assert.NotContains(t, names, "configure")
	assert.NotContains(t, names, "register")
	assert.NotContains(t, names, "acp-forward")
	assert.NotContains(t, names, "postman")
	assert.NotContains(t, names, "harnesses")
	assert.NotContains(t, names, "install-service")
}

func TestPaxdLoginCommitsRemoteAndLocalSecretRef(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	restore := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		assert.Equal(t, "prod", spec.RemoteID)
		assert.Equal(t, "https://api.example.test", spec.CloudAPIURL)
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_123",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restore()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"login",
		"--remote", "prod",
		"--cloud-url", "https://api.example.test",
	})
	require.NoError(t, err)

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	remotes, err := store.ListRemotes(context.Background(), control.ListRemotesQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, remotes, 1)

	secretPath := filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "node_key")
	assert.Equal(t, "prod", remotes[0].Remote.ID)
	assert.Equal(t, "prod", remotes[0].Remote.Name)
	assert.Equal(t, "https://api.example.test", remotes[0].Remote.CloudAPIURL)
	assert.Equal(t, "node_123", remotes[0].Remote.NodeID)
	material, err := store.GetRemoteAuthMaterial(context.Background(), "prod")
	require.NoError(t, err)
	assert.Equal(t, "file:"+secretPath, material.CloudAPIKeyRef)
	assert.NotContains(t, material.CloudAPIKeyRef, "node-secret")
	assert.FileExists(t, secretPath)
}

func TestPaxdLoginPersistsCloudflareAccessAuthFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_CLOUD_CF_CLIENT_ID", "cf-client")
	t.Setenv("PAX_CLOUD_CF_CLIENT_SECRET", "cf-secret")
	restore := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_123",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restore()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"login",
		"--remote", "prod",
		"--cloud-url", "https://api.example.test",
	})
	require.NoError(t, err)

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	material, err := store.GetRemoteAuthMaterial(context.Background(), "prod")
	require.NoError(t, err)
	require.NotNil(t, material.CloudflareAccess)
	secretPath := filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "cf_access_client_secret")
	assert.Equal(t, control.RemoteAuthCloudflareAccess, material.AuthKind)
	assert.Equal(t, "cf-client", material.CloudflareAccess.ClientID)
	assert.Equal(t, "file:"+secretPath, material.CloudflareAccess.ClientSecretRef)
	assert.NotContains(t, material.CloudflareAccess.ClientSecretRef, "cf-secret")
	data, err := os.ReadFile(secretPath)
	require.NoError(t, err)
	assert.Equal(t, "cf-secret\n", string(data))
}

func TestPaxdLoginDoesNotExposeConfigFlag(t *testing.T) {
	cmd := cmdLoginCommand()

	names := flagNames(cmd.Flags)
	assert.Contains(t, names, "remote")
	assert.Contains(t, names, "cloud-url")
	assert.Contains(t, names, "api-endpoint")
	assert.NotContains(t, names, "config")
}

func TestPaxdLoginDoesNotCommitOnLoginFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	restore := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{}, errors.New("approval denied")
	})
	defer restore()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"login",
		"--remote", "prod",
		"--cloud-url", "https://api.example.test",
	})
	require.Error(t, err)

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	remotes, err := store.ListRemotes(context.Background(), control.ListRemotesQuery{IncludeDisabled: true})
	require.NoError(t, err)
	assert.Empty(t, remotes)
	assert.NoFileExists(t, filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "node_key"))
}

func TestCommitRemoteLoginUpdatesExistingRemote(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := configWithDB(filepath.Join(home, ".paxd", "paxd.db"))
	store := openTestDaemonStore(t, cfg.Daemon.DBPath)
	enabled := true
	_, err := store.CreateRemote(context.Background(), control.CreateRemoteCommand{
		Remote: control.Remote{
			ID:          "prod",
			Name:        "Production",
			CloudAPIURL: "https://old.example.test",
			NodeID:      "node_old",
			Enabled:     &enabled,
		},
		CloudAPIKeyRef: "file:/old/key",
	})
	require.NoError(t, err)

	err = commitRemoteLogin(context.Background(), cfg, remotelogin.LoginResult{
		RemoteID:    "prod",
		CloudAPIURL: "https://api.example.test",
		NodeID:      "node_new",
		NodeAPIKey:  "new-secret",
	})
	require.NoError(t, err)

	remotes, err := store.ListRemotes(context.Background(), control.ListRemotesQuery{IncludeDisabled: true})
	require.NoError(t, err)
	require.Len(t, remotes, 1)
	assert.Equal(t, "Production", remotes[0].Remote.Name)
	assert.Equal(t, "https://api.example.test", remotes[0].Remote.CloudAPIURL)
	assert.Equal(t, "node_new", remotes[0].Remote.NodeID)

	material, err := store.GetRemoteAuthMaterial(context.Background(), "prod")
	require.NoError(t, err)
	assert.Equal(t, "file:"+filepath.Join(home, ".paxd", "secrets", "remotes", "prod", "node_key"), material.CloudAPIKeyRef)
}

func TestCommitRemoteLoginRejectsMissingRemoteID(t *testing.T) {
	cfg := configWithDB(filepath.Join(t.TempDir(), "paxd.db"))

	err := commitRemoteLogin(context.Background(), cfg, remotelogin.LoginResult{
		NodeID:     "node_123",
		NodeAPIKey: "node-secret",
	})

	require.Error(t, err)
}

func TestCommandWriterFirstNonEmptyAndVerifyCancel(t *testing.T) {
	assert.NotNil(t, commandWriter(nil))
	assert.Equal(t, "value", firstNonEmpty("", " ", "value"))
	assert.Empty(t, firstNonEmpty("", " "))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := verifyLocalAPI(ctx, time.Nanosecond)
	require.ErrorIs(t, err, context.Canceled)
}

func TestPaxdSetupLogsInInstallsStartsAndVerifies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_CLOUD_CF_CLIENT_ID", "cf-client")
	t.Setenv("PAX_CLOUD_CF_CLIENT_SECRET", "cf-secret")
	var calls []string
	restoreLogin := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		calls = append(calls, "login:"+spec.RemoteID)
		assert.Equal(t, "default", spec.RemoteID)
		assert.Equal(t, config.DefaultCloudAPIURL, spec.CloudAPIURL)
		return remotelogin.LoginResult{
			RemoteID:    spec.RemoteID,
			CloudAPIURL: spec.CloudAPIURL,
			NodeID:      "node_123",
			NodeAPIKey:  "node-secret",
		}, nil
	})
	defer restoreLogin()
	restoreService := stubServiceOps(t,
		func(opts serviceInstallOptions) error {
			calls = append(calls, "service install")
			assert.True(t, opts.Force)
			assert.True(t, opts.SuppressNextSteps)
			return nil
		},
		func(action string, system bool) error {
			calls = append(calls, "service "+action)
			assert.False(t, system)
			return nil
		},
	)
	defer restoreService()
	restoreVerify := stubVerifyLocalAPI(t, func(ctx context.Context, timeout time.Duration) error {
		calls = append(calls, "verify")
		assert.Equal(t, 20*time.Second, timeout)
		return nil
	})
	defer restoreVerify()

	app := newApp()
	var stdout bytes.Buffer
	app.Writer = &stdout
	err := app.Run(context.Background(), []string{
		"paxd",
		"setup",
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"login:default", "service install", "service restart", "verify"}, calls)
	assert.Contains(t, stdout.String(), "Connected default remote as node_123.")
	assert.Contains(t, stdout.String(), "Background service started.")
	assert.NotContains(t, stdout.String(), "Next steps:")
	assert.NotContains(t, stdout.String(), "paxd service start")
	assert.DirExists(t, filepath.Join(home, ".paxd"))

	store := openTestDaemonStore(t, filepath.Join(home, ".paxd", "paxd.db"))
	material, err := store.GetRemoteAuthMaterial(context.Background(), "default")
	require.NoError(t, err)
	require.NotNil(t, material.CloudflareAccess)
	secretPath := filepath.Join(home, ".paxd", "secrets", "remotes", "default", "cf_access_client_secret")
	assert.Equal(t, control.RemoteAuthCloudflareAccess, material.AuthKind)
	assert.Equal(t, "cf-client", material.CloudflareAccess.ClientID)
	assert.Equal(t, "file:"+secretPath, material.CloudflareAccess.ClientSecretRef)
	data, err := os.ReadFile(secretPath)
	require.NoError(t, err)
	assert.Equal(t, "cf-secret\n", string(data))
}

func TestPaxdSetupExposesOnlyCloudURLFlagWithDefault(t *testing.T) {
	cmd := cmdSetupCommand()

	require.Len(t, cmd.Flags, 1)
	flag, ok := cmd.Flags[0].(*cli.StringFlag)
	require.True(t, ok)
	assert.Equal(t, "cloud-url", flag.Names()[0])
	assert.Equal(t, config.DefaultCloudAPIURL, flag.Value)
}

func TestPaxdSetupDoesNotStartServiceWhenLoginFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	restoreLogin := stubRemoteLogin(t, func(ctx context.Context, spec remotelogin.LoginSpec, opts remotelogin.Options) (remotelogin.LoginResult, error) {
		return remotelogin.LoginResult{}, errors.New("approval denied")
	})
	defer restoreLogin()
	serviceCalled := false
	restoreService := stubServiceOps(t,
		func(opts serviceInstallOptions) error {
			serviceCalled = true
			return nil
		},
		func(action string, system bool) error {
			serviceCalled = true
			return nil
		},
	)
	defer restoreService()

	err := newApp().Run(context.Background(), []string{
		"paxd",
		"setup",
		"--cloud-url", "https://api.example.test",
	})
	require.Error(t, err)

	assert.False(t, serviceCalled)
	assert.NoFileExists(t, filepath.Join(home, ".paxd", "secrets", "remotes", "default", "node_key"))
}

func stubRemoteLogin(t *testing.T, fn remoteLoginFunc) func() {
	t.Helper()
	previous := runRemoteLogin
	runRemoteLogin = fn
	return func() {
		runRemoteLogin = previous
	}
}

func stubServiceOps(t *testing.T, install func(serviceInstallOptions) error, control func(string, bool) error) func() {
	t.Helper()
	previousInstall := installPaxdService
	previousControl := controlPaxdService
	installPaxdService = install
	controlPaxdService = control
	return func() {
		installPaxdService = previousInstall
		controlPaxdService = previousControl
	}
}

func stubVerifyLocalAPI(t *testing.T, verify func(context.Context, time.Duration) error) func() {
	t.Helper()
	previous := verifyPaxdLocalAPI
	verifyPaxdLocalAPI = verify
	return func() {
		verifyPaxdLocalAPI = previous
	}
}

func flagNames(flags []cli.Flag) []string {
	names := make([]string, 0, len(flags))
	for _, flag := range flags {
		names = append(names, flag.Names()...)
	}
	return names
}

func openTestDaemonStore(t *testing.T, path string) *daemonstore.Store {
	t.Helper()
	store, err := daemonstore.OpenSQLite(path)
	require.NoError(t, err)
	require.NoError(t, store.Migrate(context.Background()))
	t.Cleanup(func() { closeDaemonStore(store) })
	return store
}

func configWithDB(path string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Daemon.DBPath = path
	return &cfg
}
