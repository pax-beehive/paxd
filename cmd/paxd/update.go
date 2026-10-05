package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/safehttp"
	"github.com/pax-beehive/paxd/internal/updater"
	"github.com/urfave/cli/v3"
)

const defaultPaxdUpdateResolverURL = "https://api.paxworkspace.net/api/v1/public/paxd/download"
const defaultPaxdUpdateTag = "stable"

const (
	paxdUpdateStatusUpToDate    = "up_to_date"
	paxdUpdateStatusAvailable   = "update_available"
	paxdUpdateStatusAhead       = "ahead"
	paxdUpdateStatusDevelopment = "development"
	paxdUpdateStatusUnknown     = "unknown"
)

var paxdUpdateHTTPClient paxdUpdateHTTPDoer = http.DefaultClient
var paxdExecutablePath = os.Executable
var paxdUpdateResolverForRemote = defaultPaxdUpdateResolverForRemote

type paxdUpdateHTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

type paxdUpdateCheckResponse struct {
	CurrentStatus   string    `json:"current_status,omitempty"`
	Warning         string    `json:"warning,omitempty"`
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version"`
	Status          string    `json:"status"`
	UpdateAvailable bool      `json:"update_available"`
	Platform        string    `json:"platform"`
	DownloadURL     string    `json:"-"`
	SHA256          string    `json:"sha256"`
	SizeBytes       int64     `json:"size_bytes"`
	CheckedAt       time.Time `json:"checked_at"`
}

type paxdApplyUpdateResponse struct {
	Warning         string `json:"warning,omitempty"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	Status          string `json:"status"`
	UpdateAvailable bool   `json:"update_available"`
	Updated         bool   `json:"updated"`
	Path            string `json:"path,omitempty"`
	Platform        string `json:"platform"`
	SHA256          string `json:"sha256,omitempty"`
	SizeBytes       int64  `json:"size_bytes,omitempty"`
}

type paxdUpdateArtifact struct {
	CurrentStatus string
	URL           string
	SHA256        string
	Version       string
	Size          int64
}

type paxdUpdateResolverResponse struct {
	Data struct {
		CurrentStatus string   `json:"current_status"`
		Tags          []string `json:"tags"`
		URL           string   `json:"url"`
		SHA256        string   `json:"sha256"`
		Version       string   `json:"version"`
		SizeBytes     int64    `json:"size_bytes"`
		Size          int64    `json:"size"`
	} `json:"data"`
}

func cmdUpdateCommand() *cli.Command {
	updateFlags := func(timeout string) []cli.Flag {
		return []cli.Flag{
			&cli.StringFlag{Name: "format", Value: "text", Usage: "Output format: text or json"},
			&cli.StringFlag{Name: "remote", Value: "default", Usage: "local remote used to resolve paxd releases"},
			&cli.StringFlag{Name: "resolver-url", Usage: "explicit paxd artifact resolver URL override"},
			&cli.StringFlag{Name: "tag", Value: defaultPaxdUpdateTag, Usage: "Release tag to check"},
			&cli.StringFlag{Name: "platform", Usage: "Release platform override like darwin/arm64"},
			&cli.StringFlag{Name: "timeout", Value: timeout, Usage: "Update timeout"},
		}
	}
	return &cli.Command{
		Name:  "update",
		Usage: "Update the paxd binary in place",
		Flags: updateFlags("30s"),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			return paxdUpdateCommand(ctx, cmd)
		},
		Commands: []*cli.Command{{
			Name:  "check",
			Usage: "Check the latest hosted paxd release",
			Flags: updateFlags("3s"),
			Action: func(ctx context.Context, cmd *cli.Command) error {
				return paxdUpdateCheckCommand(ctx, cmd)
			},
		}},
	}
}

func paxdUpdateCheckCommand(ctx context.Context, cmd *cli.Command) error {
	runCtx, cancel, err := paxdContextWithTimeout(ctx, cmd.String("timeout"))
	if err != nil {
		return err
	}
	defer cancel()
	resp, err := paxdCheckUpdate(runCtx, cmd)
	if err != nil {
		return fmt.Errorf("check update: %w", err)
	}
	return renderPaxdUpdateCheck(commandWriter(cmd), resp, cmd.String("format"))
}

func paxdUpdateCommand(ctx context.Context, cmd *cli.Command) error {
	runCtx, cancel, err := paxdContextWithTimeout(ctx, cmd.String("timeout"))
	if err != nil {
		return err
	}
	defer cancel()
	check, err := paxdCheckUpdate(runCtx, cmd)
	if err != nil {
		return fmt.Errorf("check update: %w", err)
	}
	resp := &paxdApplyUpdateResponse{
		CurrentVersion:  check.CurrentVersion,
		Warning:         check.Warning,
		LatestVersion:   check.LatestVersion,
		Status:          check.Status,
		UpdateAvailable: check.UpdateAvailable,
		Platform:        check.Platform,
		SHA256:          check.SHA256,
		SizeBytes:       check.SizeBytes,
	}
	if !check.UpdateAvailable {
		return renderPaxdApplyUpdate(commandWriter(cmd), resp, cmd.String("format"))
	}
	binary, err := downloadPaxdUpdate(runCtx, check.DownloadURL, check.SizeBytes)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	if err := verifyPaxdUpdateBinary(binary, check.SHA256); err != nil {
		return fmt.Errorf("verify update: %w", err)
	}
	path, err := paxdExecutablePath()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	if err := replacePaxdExecutable(path, binary); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	resp.Updated = true
	resp.Path = path
	return renderPaxdApplyUpdate(commandWriter(cmd), resp, cmd.String("format"))
}

func paxdCheckUpdate(ctx context.Context, cmd *cli.Command) (*paxdUpdateCheckResponse, error) {
	platform := firstNonEmpty(cmd.String("platform"), runtime.GOOS+"/"+runtime.GOARCH)
	resolverURL, err := paxdUpdateResolverURL(ctx, cmd)
	if err != nil {
		return nil, err
	}
	artifact, err := resolvePaxdUpdateArtifact(ctx, resolverURL, platform, cmd.String("tag"))
	if err != nil {
		return nil, err
	}
	status, updateAvailable, err := comparePaxdUpdateVersions(version, artifact.Version)
	if err != nil {
		return nil, err
	}
	return &paxdUpdateCheckResponse{
		CurrentVersion:  version,
		CurrentStatus:   artifact.CurrentStatus,
		Warning:         paxdBinaryQualityWarning(artifact.CurrentStatus),
		LatestVersion:   artifact.Version,
		Status:          status,
		UpdateAvailable: updateAvailable,
		Platform:        platform,
		DownloadURL:     artifact.URL,
		SHA256:          artifact.SHA256,
		SizeBytes:       artifact.Size,
		CheckedAt:       time.Now().UTC(),
	}, nil
}

func paxdUpdateResolverURL(ctx context.Context, cmd *cli.Command) (string, error) {
	if explicitURL := strings.TrimSpace(cmd.String("resolver-url")); explicitURL != "" {
		return explicitURL, nil
	}
	remoteID := firstNonEmpty(cmd.String("remote"), "default")
	return paxdUpdateResolverForRemote(ctx, strings.TrimSpace(remoteID))
}

func defaultPaxdUpdateResolverForRemote(ctx context.Context, remoteID string) (string, error) {
	remoteID = firstNonEmpty(strings.TrimSpace(remoteID), "default")
	cfg, err := loadRuntimeConfig()
	if err != nil {
		return "", fmt.Errorf("load paxd config for update resolver: %w", err)
	}
	databasePath := strings.TrimSpace(cfg.Daemon.DBPath)
	if databasePath == "" {
		return fallbackPaxdUpdateResolver(remoteID)
	}
	if _, err := os.Stat(databasePath); err != nil {
		if os.IsNotExist(err) {
			return fallbackPaxdUpdateResolver(remoteID)
		}
		return "", fmt.Errorf("stat paxd database for update resolver: %w", err)
	}
	store, err := daemonstore.OpenSQLite(databasePath)
	if err != nil {
		return "", fmt.Errorf("open paxd database for update resolver: %w", err)
	}
	defer func() { _ = store.Close() }()
	remotes, err := store.ListRemotes(ctx, control.ListRemotesQuery{IncludeDisabled: true})
	if err != nil {
		return "", fmt.Errorf("list local remotes for update resolver: %w", err)
	}
	for _, remote := range remotes {
		if strings.TrimSpace(remote.Remote.ID) != remoteID {
			continue
		}
		resolverURL, err := updater.ResolverURLFromCloudAPIURL(remote.Remote.CloudAPIURL)
		if err != nil {
			return "", fmt.Errorf("derive update resolver for remote %q: %w", remoteID, err)
		}
		return resolverURL, nil
	}
	return fallbackPaxdUpdateResolver(remoteID)
}

func fallbackPaxdUpdateResolver(remoteID string) (string, error) {
	if remoteID == "default" {
		return defaultPaxdUpdateResolverURL, nil
	}
	return "", fmt.Errorf("local remote %q does not exist", remoteID)
}

func resolvePaxdUpdateArtifact(
	ctx context.Context,
	resolverURL string,
	platform string,
	tag string,
) (*paxdUpdateArtifact, error) {
	endpoint, err := url.Parse(resolverURL)
	if err != nil {
		return nil, safehttp.RedactError("parse paxd update resolver URL", err)
	}
	query := endpoint.Query()
	query.Set("platform", platform)
	query.Set("current_version", version)
	query.Set("tags", firstNonEmpty(tag, defaultPaxdUpdateTag))
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil) // #nosec G107
	if err != nil {
		return nil, safehttp.RedactError("create paxd update resolver request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "paxd-update")
	resp, err := safehttp.DoNoRedirect(paxdUpdateHTTPClient, req)
	if err != nil {
		return nil, safehttp.RedactError("request paxd update resolver", err)
	}
	defer closePaxdUpdateBody(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if resp.StatusCode == http.StatusGone {
			return nil, fmt.Errorf("Current binary has known issues. No replacement is available; upgrade when a verified version is published.")
		}
		return nil, fmt.Errorf("resolver returned HTTP %d", resp.StatusCode)
	}
	var resolverResp paxdUpdateResolverResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&resolverResp); err != nil {
		return nil, fmt.Errorf("decode resolver response: %w", err)
	}
	if slices.Contains(resolverResp.Data.Tags, "disabled") {
		return nil, fmt.Errorf("Refusing disabled binary with known issues.")
	}
	size := resolverResp.Data.SizeBytes
	if size == 0 {
		size = resolverResp.Data.Size
	}
	artifact := &paxdUpdateArtifact{
		CurrentStatus: resolverResp.Data.CurrentStatus,
		URL:           normalizePaxdArtifactURL(resolverResp.Data.URL),
		SHA256:        strings.TrimSpace(resolverResp.Data.SHA256),
		Version:       strings.TrimSpace(resolverResp.Data.Version),
		Size:          size,
	}
	if artifact.Version == "" {
		return nil, fmt.Errorf("resolver version is required")
	}
	if artifact.URL == "" {
		return nil, fmt.Errorf("resolver download URL is required")
	}
	if artifact.SHA256 == "" {
		return nil, fmt.Errorf("resolver sha256 is required")
	}
	return artifact, nil
}

func downloadPaxdUpdate(ctx context.Context, rawURL string, expectedSize int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil) // #nosec G107
	if err != nil {
		return nil, safehttp.RedactError("create paxd update download request", err)
	}
	req.Header.Set("User-Agent", "paxd-update")
	resp, err := safehttp.DoNoRedirect(paxdUpdateHTTPClient, req)
	if err != nil {
		return nil, safehttp.RedactError("request download", err)
	}
	defer closePaxdUpdateBody(resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	limit := expectedSize + 1
	if limit <= 1 {
		limit = 256 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read download: %w", err)
	}
	if expectedSize > 0 && int64(len(body)) != expectedSize {
		return nil, fmt.Errorf("download size %d does not match expected %d", len(body), expectedSize)
	}
	return body, nil
}

func verifyPaxdUpdateBinary(binary []byte, expectedSHA string) error {
	sum := sha256.Sum256(binary)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, strings.TrimSpace(expectedSHA)) {
		return fmt.Errorf("sha256 %s does not match expected %s", got, expectedSHA)
	}
	return nil
}

func comparePaxdUpdateVersions(current string, latest string) (string, bool, error) {
	currentVersion, ok := parseOptionalPaxdSemver(current)
	if !ok {
		return paxdUpdateStatusDevelopment, false, nil
	}
	latestVersion, err := parsePaxdSemver(latest)
	if err != nil {
		return paxdUpdateStatusUnknown, false, fmt.Errorf("parse latest version: %w", err)
	}
	switch currentVersion.compare(latestVersion) {
	case -1:
		return paxdUpdateStatusAvailable, true, nil
	case 0:
		return paxdUpdateStatusUpToDate, false, nil
	default:
		return paxdUpdateStatusAhead, false, nil
	}
}

func parseOptionalPaxdSemver(raw string) (*paxdSemver, bool) {
	version, err := parsePaxdSemver(raw)
	if err != nil {
		return nil, false
	}
	return version, true
}

type paxdSemver struct {
	major int
	minor int
	patch int
}

func parsePaxdSemver(raw string) (*paxdSemver, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(raw), "v")
	parts := strings.Split(clean, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("version %q is not semantic", raw)
	}
	major, err := parsePaxdSemverPart(parts[0], raw)
	if err != nil {
		return nil, err
	}
	minor, err := parsePaxdSemverPart(parts[1], raw)
	if err != nil {
		return nil, err
	}
	patch, err := parsePaxdSemverPart(parts[2], raw)
	if err != nil {
		return nil, err
	}
	return &paxdSemver{major: major, minor: minor, patch: patch}, nil
}

func parsePaxdSemverPart(part string, raw string) (int, error) {
	if part == "" {
		return 0, fmt.Errorf("version %q is not semantic", raw)
	}
	value, err := strconv.Atoi(part)
	if err != nil {
		return 0, fmt.Errorf("version %q is not semantic: %w", raw, err)
	}
	if value < 0 {
		return 0, fmt.Errorf("version %q is not semantic", raw)
	}
	return value, nil
}

func (v *paxdSemver) compare(other *paxdSemver) int {
	if v.major != other.major {
		return comparePaxdInt(v.major, other.major)
	}
	if v.minor != other.minor {
		return comparePaxdInt(v.minor, other.minor)
	}
	return comparePaxdInt(v.patch, other.patch)
}

func comparePaxdInt(left int, right int) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func replacePaxdExecutable(path string, binary []byte) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("executable path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat executable: %w", err)
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(dir, "."+base+".update-*")
	if err != nil {
		return fmt.Errorf("create temp executable: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(binary); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp executable: %w", err)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp executable: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace executable: %w", err)
	}
	cleanup = false
	return nil
}

func renderPaxdUpdateCheck(stdout io.Writer, resp *paxdUpdateCheckResponse, format string) error {
	switch format {
	case "text":
		if resp.Warning != "" {
			fmt.Fprintln(stdout, "Warning: "+resp.Warning)
		}
		fmt.Fprintf(stdout, "Current: %s\n", resp.CurrentVersion)
		fmt.Fprintf(stdout, "Latest:  %s\n", resp.LatestVersion)
		fmt.Fprintf(stdout, "Status:  %s\n", resp.Status)
		return nil
	case "json":
		return json.NewEncoder(stdout).Encode(resp)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

func renderPaxdApplyUpdate(stdout io.Writer, resp *paxdApplyUpdateResponse, format string) error {
	switch format {
	case "text":
		if resp.Warning != "" {
			if _, err := fmt.Fprintln(stdout, "Warning: "+resp.Warning); err != nil {
				return err
			}
		}
		if resp.Updated {
			fmt.Fprintf(stdout, "Updated paxd %s -> %s\n", resp.CurrentVersion, resp.LatestVersion)
			fmt.Fprintf(stdout, "Path: %s\n", resp.Path)
			return nil
		}
		if resp.Status == paxdUpdateStatusUpToDate {
			fmt.Fprintf(stdout, "paxd is already up to date (%s).\n", resp.CurrentVersion)
			return nil
		}
		fmt.Fprintf(stdout, "No paxd update applied (%s, status: %s).\n", resp.CurrentVersion, resp.Status)
		return nil
	case "json":
		return json.NewEncoder(stdout).Encode(resp)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

func paxdContextWithTimeout(ctx context.Context, raw string) (context.Context, context.CancelFunc, error) {
	timeout, err := time.ParseDuration(firstNonEmpty(raw, "30s"))
	if err != nil {
		return nil, nil, fmt.Errorf("parse timeout: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	return runCtx, cancel, nil
}

func normalizePaxdArtifactURL(raw string) string {
	if !strings.HasPrefix(raw, "gs://") {
		return raw
	}
	rest := strings.TrimPrefix(raw, "gs://")
	bucket, object, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" {
		return raw
	}
	return "https://storage.googleapis.com/" + bucket + "/" + object
}

func closePaxdUpdateBody(body io.Closer) {
	if body != nil {
		_ = body.Close()
	}
}

func paxdBinaryQualityWarning(state string) string {
	if state == "disabled" {
		return "Current version has known issues. Upgrade to a verified stable version as soon as possible."
	}
	return ""
}
