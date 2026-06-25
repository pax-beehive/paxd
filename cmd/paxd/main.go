// paxd is the local Pax daemon. It starts even before remotes, credentials, or
// agent connections exist; those records are managed later through the local
// control API.
//
// Architecture:
//
//	local clients --HTTP/Unix socket--> paxd --daemonstore--> supervisors
//
// Usage:
//
//	paxd run              # start the daemon
//	paxd service install  # install as launchd/systemd service
//	paxd --version        # print version
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/config"
	"github.com/pax-beehive/paxd/internal/control"
	paxdaemon "github.com/pax-beehive/paxd/internal/daemon"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/localapi"
	"github.com/pax-beehive/paxd/internal/remotelogin"
	"github.com/pax-beehive/paxd/internal/remotesecrets"
	"github.com/pax-beehive/paxd/internal/state"
	"github.com/urfave/cli/v3"
)

var version = "0.1.0"

type remoteLoginFunc func(context.Context, remotelogin.LoginSpec, remotelogin.Options) (remotelogin.LoginResult, error)

var runRemoteLogin remoteLoginFunc = remotelogin.Login
var installPaxdService = serviceInstall
var controlPaxdService = serviceControl
var verifyPaxdLocalAPI = verifyLocalAPI

func main() {
	if err := newApp().Run(context.Background(), os.Args); err != nil {
		log.Fatal(err)
	}
}

func newApp() *cli.Command {
	return &cli.Command{
		Name:    "paxd",
		Usage:   "Pax Fleet Daemon",
		Version: version,
		Commands: []*cli.Command{
			cmdSetupCommand(),
			cmdLoginCommand(),
			{
				Name:            "run",
				Usage:           "start the daemon",
				SkipFlagParsing: true,
				Action: func(ctx context.Context, cmd *cli.Command) error {
					cmdRun(cmd.Args().Slice())
					return nil
				},
			},
			cmdServiceCommand(),
		},
	}
}

func cmdLoginCommand() *cli.Command {
	return &cli.Command{
		Name:  "login",
		Usage: "login this machine to a Pax remote",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "remote", Value: "default", Usage: "local remote id"},
			&cli.StringFlag{Name: "cloud-url", Value: config.DefaultCloudAPIURL, Usage: "Pax cloud API URL"},
			&cli.StringFlag{Name: "api-endpoint", Usage: "optional local API endpoint advertised for this node"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := loadRuntimeConfig()
			if err != nil {
				return err
			}
			cloudURL := cloudURLFromCommand(cmd, cfg)
			client, err := loginCloudClient(cloudURL, cfg)
			if err != nil {
				return err
			}
			remoteID := strings.TrimSpace(cmd.String("remote"))
			result, err := runRemoteLogin(ctx, remotelogin.LoginSpec{
				RemoteID:    remoteID,
				CloudAPIURL: cloudURL,
				Node: remotelogin.NodeRegistrationInfo{
					Name:        cfg.Agent.Name,
					Hostname:    cfg.Agent.Hostname,
					MachineType: cfg.Agent.MachineType,
					OS:          runtime.GOOS,
					Arch:        runtime.GOARCH,
					PaxdVersion: version,
					APIEndpoint: cmd.String("api-endpoint"),
				},
			}, remotelogin.Options{
				Client: client,
				Stdout: commandWriter(cmd),
			})
			if err != nil {
				return err
			}
			if err := commitRemoteLogin(ctx, cfg, result); err != nil {
				return err
			}
			fmt.Fprintf(commandWriter(cmd), "Connected %s remote as %s.\n", result.RemoteID, result.NodeID)
			return nil
		},
	}
}

func cmdSetupCommand() *cli.Command {
	return &cli.Command{
		Name:  "setup",
		Usage: "login this machine and start paxd as a background service",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "cloud-url", Value: config.DefaultCloudAPIURL, Usage: "Pax cloud API URL"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return cmdSetup(ctx, cmd)
		},
	}
}

func cmdSetup(ctx context.Context, cmd *cli.Command) error {
	if err := ensurePaxdHome(); err != nil {
		return err
	}
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return err
	}
	cloudURL := cloudURLFromCommand(cmd, cfg)
	client, err := loginCloudClient(cloudURL, cfg)
	if err != nil {
		return err
	}
	remoteID := "default"
	result, err := runRemoteLogin(ctx, remotelogin.LoginSpec{
		RemoteID:    remoteID,
		CloudAPIURL: cloudURL,
		Node: remotelogin.NodeRegistrationInfo{
			Name:        cfg.Agent.Name,
			Hostname:    cfg.Agent.Hostname,
			MachineType: cfg.Agent.MachineType,
			OS:          runtime.GOOS,
			Arch:        runtime.GOARCH,
			PaxdVersion: version,
		},
	}, remotelogin.Options{
		Client: client,
		Stdout: commandWriter(cmd),
	})
	if err != nil {
		return err
	}
	if err := commitRemoteLogin(ctx, cfg, result); err != nil {
		return err
	}
	fmt.Fprintf(commandWriter(cmd), "Connected %s remote as %s.\n", result.RemoteID, result.NodeID)

	installOpts := serviceInstallOptions{
		Force:             true,
		SuppressNextSteps: true,
	}
	if err := installPaxdService(installOpts); err != nil {
		return err
	}
	if err := controlPaxdService("restart", installOpts.System); err != nil {
		return err
	}
	fmt.Fprintln(commandWriter(cmd), "Background service started.")
	if err := verifyPaxdLocalAPI(ctx, 20*time.Second); err != nil {
		return err
	}
	return nil
}

func commandWriter(cmd *cli.Command) io.Writer {
	if cmd != nil && cmd.Writer != nil {
		return cmd.Writer
	}
	return os.Stdout
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func loadRuntimeConfig() (*config.Config, error) {
	cfg, err := config.Load("")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.Agent.Hostname) == "" {
		host, _ := os.Hostname()
		cfg.Agent.Hostname = host
	}
	return cfg, nil
}

func cloudURLFromCommand(cmd *cli.Command, cfg *config.Config) string {
	flagURL := strings.TrimSpace(cmd.String("cloud-url"))
	cfgURL := ""
	if cfg != nil {
		cfgURL = strings.TrimSpace(cfg.Cloud.APIURL)
	}
	if flagURL == "" || flagURL == config.DefaultCloudAPIURL {
		return strings.TrimRight(firstNonEmpty(cfgURL, flagURL, config.DefaultCloudAPIURL), "/")
	}
	return strings.TrimRight(flagURL, "/")
}

func loginCloudClient(cloudURL string, cfg *config.Config) (*cloud.Client, error) {
	client := cloud.NewClient(cloudURL, "")
	clientID, clientSecret, enabled, err := cloudflareAccessCredentials(cfg)
	if err != nil {
		return nil, err
	}
	if enabled {
		client.WithCloudflareAccess(clientID, clientSecret)
	}
	return client, nil
}

func cloudflareAccessCredentials(cfg *config.Config) (string, string, bool, error) {
	if cfg == nil {
		return "", "", false, nil
	}
	clientID := strings.TrimSpace(cfg.Cloud.CFClientID)
	clientSecret := strings.TrimSpace(cfg.Cloud.CFClientSecret)
	if clientID == "" && clientSecret == "" {
		return "", "", false, nil
	}
	if clientID == "" || clientSecret == "" {
		return "", "", false, fmt.Errorf("cloudflare access setup requires both PAX_CLOUD_CF_CLIENT_ID and PAX_CLOUD_CF_CLIENT_SECRET")
	}
	return clientID, clientSecret, true, nil
}

func commitRemoteLogin(ctx context.Context, cfg *config.Config, result remotelogin.LoginResult) error {
	remoteID := strings.TrimSpace(result.RemoteID)
	if remoteID == "" {
		return fmt.Errorf("remote id is required")
	}
	secrets := remotesecrets.Store{}
	secretRef, err := secrets.StoreNodeKey(ctx, remoteID, result.NodeAPIKey)
	if err != nil {
		return err
	}
	store, closeStore, err := openDaemonStore(cfg.Daemon.DBPath)
	if err != nil {
		return err
	}
	defer closeStore()

	enabled := true
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return err
	}
	for _, remote := range remotes {
		if remote.Remote.ID != remoteID {
			continue
		}
		name := firstNonEmpty(remote.Remote.Name, remoteID)
		cloudURL := result.CloudAPIURL
		nodeID := result.NodeID
		_, err := store.UpdateRemote(ctx, control.UpdateRemoteCommand{
			RemoteID: remoteID,
			Remote: control.RemotePatch{
				Name:        &name,
				CloudAPIURL: &cloudURL,
				NodeID:      &nodeID,
				Enabled:     &enabled,
			},
			CloudAPIKeyRef: &secretRef,
		})
		if err != nil {
			return err
		}
		return configureRemoteAuthFromConfig(ctx, store, secrets, remoteID, cfg)
	}
	_, err = store.CreateRemote(ctx, control.CreateRemoteCommand{
		Remote: control.Remote{
			ID:          remoteID,
			Name:        remoteID,
			CloudAPIURL: result.CloudAPIURL,
			NodeID:      result.NodeID,
			Enabled:     &enabled,
		},
		CloudAPIKeyRef: secretRef,
	})
	if err != nil {
		return err
	}
	return configureRemoteAuthFromConfig(ctx, store, secrets, remoteID, cfg)
}

func configureRemoteAuthFromConfig(ctx context.Context, store *daemonstore.Store, secrets remotesecrets.Store, remoteID string, cfg *config.Config) error {
	clientID, clientSecret, enabled, err := cloudflareAccessCredentials(cfg)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	clientSecretRef, err := secrets.StoreCloudflareAccessClientSecret(ctx, remoteID, clientSecret)
	if err != nil {
		return err
	}
	return store.ConfigureRemoteAuth(ctx, control.ConfigureRemoteAuthCommand{
		RemoteID: remoteID,
		Kind:     control.RemoteAuthCloudflareAccess,
		CloudflareAccess: &control.CloudflareAccessAuth{
			ClientID:        clientID,
			ClientSecretRef: clientSecretRef,
		},
	})
}

func ensurePaxdHome() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("get home dir: %w", err)
	}
	return os.MkdirAll(filepath.Join(home, ".paxd"), 0700)
}

func verifyLocalAPI(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := localapi.NewUnixClient(paxdaemon.DefaultControlSocketPath())
	var lastErr error
	for {
		if _, err := client.GetStatus(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("verify local control API: %w", lastErr)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// cmdRun starts the always-on local daemon. Remote and agent runtime state is
// driven from daemonstore desired state, not from registration gates.
func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	controlSocket := fs.String("control-socket", paxdaemon.DefaultControlSocket, "Unix socket path for local control API, or empty/none to disable")
	debugHTTP := fs.String("debug-http", "", "optional loopback debug HTTP address, for example 127.0.0.1:8765")
	fs.Parse(args)

	cfg, err := loadRuntimeConfig()
	if err != nil {
		log.Fatalf("load runtime config: %v", err)
	}

	log.Printf("[paxd] starting v%s on %s/%s", version, runtime.GOOS, runtime.GOARCH)

	sm := state.NewMachine()
	history, closeHistory, err := openDaemonStore(cfg.Daemon.DBPath)
	if err != nil {
		log.Fatalf("open daemonstore: %v", err)
	}
	defer closeHistory()

	daemonRuntime, err := paxdaemon.Bootstrap(sm.Context(), paxdaemon.Options{
		Config: cfg,
		Store:  history,
	})
	if err != nil {
		log.Fatalf("bootstrap daemon control plane: %v", err)
	}

	if strings.TrimSpace(*controlSocket) != "" && strings.TrimSpace(*controlSocket) != "none" {
		localServer, err := paxdaemon.StartUnixLocalAPI(sm.Context(), *controlSocket, daemonRuntime.LocalHandler)
		if err != nil {
			log.Fatalf("start local control API: %v", err)
		}
		defer localServer.Close()
		log.Printf("[paxd] local control API listening on unix://%s", localServer.Addr())
	}
	if strings.TrimSpace(*debugHTTP) != "" {
		debugServer, err := paxdaemon.StartDebugHTTP(sm.Context(), *debugHTTP, daemonRuntime.LocalHandler)
		if err != nil {
			log.Fatalf("start debug HTTP API: %v", err)
		}
		if debugServer != nil {
			defer debugServer.Close()
			log.Printf("[paxd] debug control API listening on http://%s", debugServer.Addr())
		}
	}
	daemonRuntime.StartSupervisors(sm.Context())

	if err := sm.Transition(state.RUNNING); err != nil {
		log.Fatalf("state transition: %v", err)
	}
	log.Printf("[paxd] RUNNING")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	sig := <-sigCh
	log.Printf("[paxd] received signal: %v", sig)
	_ = sm.Transition(state.STOPPING)
	_ = sm.Transition(state.STOPPED)
	log.Printf("[paxd] STOPPED")
}

func openDaemonStore(path string) (*daemonstore.Store, func(), error) {
	history, err := daemonstore.OpenSQLite(path)
	if err != nil {
		return nil, nil, err
	}
	if err := history.Migrate(context.Background()); err != nil {
		closeDaemonStore(history)
		return nil, nil, err
	}
	return history, func() { closeDaemonStore(history) }, nil
}

func closeDaemonStore(store *daemonstore.Store) {
	if store == nil {
		return
	}
	db, err := store.DB().DB()
	if err == nil {
		_ = db.Close()
	}
}

const (
	paxdServiceName   = "paxd"
	paxdLaunchdLabel  = "com.paxtech.paxd"
	paxdServiceLogDir = ".paxd/logs"
)

var legacyPaxdLaunchdLabels = []string{"com.toddzheng.paxd"}

type serviceInstallOptions struct {
	Force             bool
	System            bool
	RunAsUser         string
	ControlSocket     string
	DebugHTTP         string
	SuppressNextSteps bool
}

type serviceTarget struct {
	GOOS     string
	ExecPath string
	Home     string
}

func cmdServiceCommand() *cli.Command {
	return &cli.Command{
		Name:  "service",
		Usage: "manage paxd as a background service",
		Commands: []*cli.Command{
			{
				Name:  "install",
				Usage: "install launchd/systemd service",
				Flags: serviceInstallFlags(),
				Action: func(ctx context.Context, cmd *cli.Command) error {
					opts := serviceInstallOptions{
						Force:         cmd.Bool("force"),
						System:        cmd.Bool("system"),
						RunAsUser:     cmd.String("run-as-user"),
						ControlSocket: cmd.String("control-socket"),
						DebugHTTP:     cmd.String("debug-http"),
					}
					return installPaxdService(opts)
				},
			},
			serviceLifecycleCommand("start"),
			serviceLifecycleCommand("stop"),
			serviceLifecycleCommand("restart"),
			serviceLifecycleCommand("status"),
			serviceLifecycleCommand("uninstall"),
		},
	}
}

func serviceInstallFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{Name: "force", Usage: "overwrite existing service definition"},
		&cli.BoolFlag{Name: "system", Usage: "install as a Linux system service"},
		&cli.StringFlag{Name: "run-as-user", Usage: "user account for Linux system service"},
		&cli.StringFlag{Name: "control-socket", Usage: "Unix socket path passed to paxd run"},
		&cli.StringFlag{Name: "debug-http", Usage: "loopback debug HTTP address passed to paxd run"},
	}
}

func serviceLifecycleCommand(action string) *cli.Command {
	return &cli.Command{
		Name:  action,
		Usage: action + " installed service",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "system", Usage: "target Linux system service"},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return controlPaxdService(action, cmd.Bool("system"))
		},
	}
}

func serviceTargetFromRuntime() (serviceTarget, error) {
	execPath, err := os.Executable()
	if err != nil {
		return serviceTarget{}, fmt.Errorf("get executable path: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return serviceTarget{}, fmt.Errorf("get home dir: %w", err)
	}
	return serviceTarget{GOOS: runtime.GOOS, ExecPath: execPath, Home: home}, nil
}

func serviceInstall(opts serviceInstallOptions) error {
	target, err := serviceTargetFromRuntime()
	if err != nil {
		return err
	}
	switch target.GOOS {
	case "darwin":
		if opts.System {
			return fmt.Errorf("--system is only supported on Linux")
		}
		return installLaunchdService(target, opts)
	case "linux":
		return installSystemdService(target, opts)
	default:
		return fmt.Errorf("background service install is not supported on %s", target.GOOS)
	}
}

func serviceControl(action string, system bool) error {
	target, err := serviceTargetFromRuntime()
	if err != nil {
		return err
	}
	switch target.GOOS {
	case "darwin":
		if system {
			return fmt.Errorf("--system is only supported on Linux")
		}
		return launchdControl(target, action)
	case "linux":
		return systemdControl(action, system)
	default:
		return fmt.Errorf("background service control is not supported on %s", target.GOOS)
	}
}

func installLaunchdService(target serviceTarget, opts serviceInstallOptions) error {
	if err := cleanupLegacyLaunchdServices(target); err != nil {
		return err
	}
	plistPath := launchdPlistPath(target.Home)
	if !opts.Force && fileExists(plistPath) {
		fmt.Printf("Service already installed at: %s\n", plistPath)
		fmt.Println("Use --force to reinstall")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return fmt.Errorf("create launch agents dir: %w", err)
	}
	if err := os.MkdirAll(serviceLogDir(target.Home), 0755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(generateLaunchdPlist(target, opts)), 0644); err != nil {
		return fmt.Errorf("write launchd plist: %w", err)
	}
	fmt.Printf("LaunchAgent installed at: %s\n", plistPath)
	if opts.SuppressNextSteps {
		return nil
	}
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Println("  paxd service start")
	fmt.Println("  paxd service status")
	fmt.Printf("  tail -f %s\n", filepath.Join(serviceLogDir(target.Home), "paxd.log"))
	return nil
}

func installSystemdService(target serviceTarget, opts serviceInstallOptions) error {
	if opts.System && os.Geteuid() != 0 {
		return fmt.Errorf("system service install requires root; re-run with sudo")
	}
	unitPath := systemdUnitPath(target.Home, opts.System)
	if !opts.Force && fileExists(unitPath) {
		fmt.Printf("Service already installed at: %s\n", unitPath)
		fmt.Println("Use --force to reinstall")
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0755); err != nil {
		return fmt.Errorf("create systemd unit dir: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(generateSystemdUnit(target, opts)), 0644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}
	if err := runCommandArgs(systemctlArgs("daemon-reload", opts.System)); err != nil {
		return err
	}
	if err := runCommandArgs(systemctlArgs("enable", opts.System, paxdServiceName)); err != nil {
		return err
	}
	fmt.Printf("Systemd %s service installed at: %s\n", serviceScope(opts.System), unitPath)
	if opts.SuppressNextSteps {
		return nil
	}
	fmt.Println()
	fmt.Println("Next steps:")
	prefix := ""
	scope := ""
	if opts.System {
		prefix = "sudo "
		scope = " --system"
	}
	fmt.Printf("  %spaxd service start%s\n", prefix, scope)
	fmt.Printf("  %spaxd service status%s\n", prefix, scope)
	fmt.Printf("  %sjournalctl%s -u %s -f\n", prefix, journalctlScope(opts.System), paxdServiceName)
	return nil
}

func launchdControl(target serviceTarget, action string) error {
	plistPath := launchdPlistPath(target.Home)
	label := paxdLaunchdLabel
	switch action {
	case "start":
		_ = runCommandQuiet("launchctl", "bootout", launchdDomain()+"/"+label)
		return launchdBootstrap(plistPath)
	case "stop":
		return runCommand("launchctl", "bootout", launchdDomain()+"/"+label)
	case "restart":
		_ = runCommandQuiet("launchctl", "bootout", launchdDomain()+"/"+label)
		return launchdBootstrap(plistPath)
	case "status":
		return runCommand("launchctl", "print", launchdDomain()+"/"+label)
	case "uninstall":
		for _, label := range append([]string{paxdLaunchdLabel}, legacyPaxdLaunchdLabels...) {
			_ = runCommandQuiet("launchctl", "bootout", launchdDomain()+"/"+label)
			path := launchdPlistPathForLabel(target.Home, label)
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove launchd plist: %w", err)
			}
		}
		fmt.Printf("LaunchAgent removed: %s\n", plistPath)
		return nil
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}

func launchdBootstrap(plistPath string) error {
	const attempts = 10
	for attempt := 0; attempt < attempts; attempt++ {
		if err := runCommandQuiet("launchctl", "bootstrap", launchdDomain(), plistPath); err == nil {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return runCommand("launchctl", "bootstrap", launchdDomain(), plistPath)
}

func cleanupLegacyLaunchdServices(target serviceTarget) error {
	for _, label := range legacyPaxdLaunchdLabels {
		plistPath := launchdPlistPathForLabel(target.Home, label)
		if !fileExists(plistPath) {
			continue
		}
		_ = runCommandQuiet("launchctl", "bootout", launchdDomain()+"/"+label)
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove legacy launchd plist: %w", err)
		}
		fmt.Printf("Legacy LaunchAgent removed: %s\n", plistPath)
	}
	return nil
}

func systemdControl(action string, system bool) error {
	switch action {
	case "start", "stop", "restart", "status":
		return runCommandArgs(systemctlArgs(action, system, paxdServiceName))
	case "uninstall":
		_ = runCommandArgs(systemctlArgs("stop", system, paxdServiceName))
		_ = runCommandArgs(systemctlArgs("disable", system, paxdServiceName))
		target, err := serviceTargetFromRuntime()
		if err != nil {
			return err
		}
		unitPath := systemdUnitPath(target.Home, system)
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove systemd unit: %w", err)
		}
		if err := runCommandArgs(systemctlArgs("daemon-reload", system)); err != nil {
			return err
		}
		fmt.Printf("Systemd %s service removed: %s\n", serviceScope(system), unitPath)
		return nil
	default:
		return fmt.Errorf("unknown service action %q", action)
	}
}

var runCommand = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

var runCommandQuiet = func(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func runCommandArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("missing command")
	}
	return runCommand(args[0], args[1:]...)
}

func generateLaunchdPlist(target serviceTarget, opts serviceInstallOptions) string {
	args := serviceRunArgs(target.ExecPath, opts)
	var programArgs strings.Builder
	for _, arg := range args {
		programArgs.WriteString("        <string>")
		programArgs.WriteString(xmlEscape(arg))
		programArgs.WriteString("</string>\n")
	}
	logDir := serviceLogDir(target.Home)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
%s    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, paxdLaunchdLabel, programArgs.String(), xmlEscape(target.Home), xmlEscape(servicePATH(target.Home)), xmlEscape(filepath.Join(logDir, "paxd.log")), xmlEscape(filepath.Join(logDir, "paxd.error.log")))
}

func generateSystemdUnit(target serviceTarget, opts serviceInstallOptions) string {
	args := serviceRunArgs(target.ExecPath, opts)
	lines := []string{
		"[Unit]",
		"Description=Pax Fleet Daemon",
		"After=network-online.target",
		"Wants=network-online.target",
		"StartLimitIntervalSec=0",
		"",
		"[Service]",
		"Type=simple",
	}
	if opts.System {
		user := strings.TrimSpace(opts.RunAsUser)
		if user == "" {
			user = defaultRunAsUser()
		}
		if user != "" {
			lines = append(lines, "User="+user)
		}
	}
	lines = append(lines,
		"ExecStart="+joinSystemdArgs(args),
		"WorkingDirectory="+quoteSystemdArg(target.Home),
		"Restart=always",
		"RestartSec=5",
		"KillMode=mixed",
		"KillSignal=SIGTERM",
		"TimeoutStopSec=90",
		"StandardOutput=journal",
		"StandardError=journal",
		"",
		"[Install]",
	)
	if opts.System {
		lines = append(lines, "WantedBy=multi-user.target")
	} else {
		lines = append(lines, "WantedBy=default.target")
	}
	return strings.Join(lines, "\n") + "\n"
}

func serviceRunArgs(execPath string, opts serviceInstallOptions) []string {
	args := []string{execPath, "run"}
	if opts.ControlSocket != "" {
		args = append(args, "--control-socket", opts.ControlSocket)
	}
	if opts.DebugHTTP != "" {
		args = append(args, "--debug-http", opts.DebugHTTP)
	}
	return args
}

func servicePATH(home string) string {
	parts := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".local", "share", "fnm", "aliases", "default", "bin"),
		filepath.Join(home, "bin"),
		"/opt/homebrew/bin",
		"/opt/homebrew/sbin",
		"/usr/local/bin",
		"/usr/local/sbin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}
	return strings.Join(parts, ":")
}

func systemctlArgs(action string, system bool, rest ...string) []string {
	args := []string{"systemctl"}
	if !system {
		args = append(args, "--user")
	}
	args = append(args, action)
	return append(args, rest...)
}

func launchdPlistPath(home string) string {
	return launchdPlistPathForLabel(home, paxdLaunchdLabel)
}

func launchdPlistPathForLabel(home string, label string) string {
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func systemdUnitPath(home string, system bool) string {
	if system {
		return filepath.Join(string(filepath.Separator), "etc", "systemd", "system", paxdServiceName+".service")
	}
	return filepath.Join(home, ".config", "systemd", "user", paxdServiceName+".service")
}

func serviceLogDir(home string) string {
	return filepath.Join(home, filepath.FromSlash(paxdServiceLogDir))
}

func serviceScope(system bool) string {
	if system {
		return "system"
	}
	return "user"
}

func journalctlScope(system bool) string {
	if system {
		return ""
	}
	return " --user"
}

func launchdDomain() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func joinSystemdArgs(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, quoteSystemdArg(arg))
	}
	return strings.Join(quoted, " ")
}

func quoteSystemdArg(arg string) string {
	return strconv.Quote(arg)
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(value)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func defaultRunAsUser() string {
	for _, key := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" && value != "root" {
			return value
		}
	}
	return ""
}
