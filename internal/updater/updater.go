package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/safehttp"
)

const (
	DefaultTag    = "stable"
	maxBinarySize = int64(256 << 20)
	resolverPath  = "/api/v1/public/paxd/download"
)

type ResolverURLForRemoteFunc func(context.Context, string) (string, error)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type Options struct {
	ResolverURL          string
	ResolverURLForRemote ResolverURLForRemoteFunc
	CurrentVersion       string
	StateDir             string
	HTTPClient           HTTPDoer
	ExecutablePath       func() (string, error)
	SmokeTimeout         time.Duration
	Now                  func() time.Time
}

type Updater struct {
	resolverURL          string
	resolverURLForRemote ResolverURLForRemoteFunc
	currentVersion       string
	stateDir             string
	httpClient           HTTPDoer
	executablePath       func() (string, error)
	smokeTimeout         time.Duration
	now                  func() time.Time
}

type Request struct {
	CommandID string
	RemoteID  string
	Version   string
	Tag       string
}

type Candidate struct {
	CommandID        string
	Path             string
	ExecutablePath   string
	OldVersion       string
	Version          string
	SHA256           string
	Size             int64
	OriginalFileMode os.FileMode
}

type ActivationRecord struct {
	CommandID       string    `json:"command_id"`
	RequestedBootID string    `json:"requested_boot_id,omitempty"`
	OldVersion      string    `json:"old_version"`
	NewVersion      string    `json:"new_version"`
	SHA256          string    `json:"sha256"`
	ExecutablePath  string    `json:"executable_path"`
	PreviousPath    string    `json:"previous_path"`
	ActivatedAt     time.Time `json:"activated_at"`
}

// ActivationCommittedError means the executable rename succeeded but a
// durability or record step failed. Callers must continue shutdown instead of
// reopening admission because the next process start will use the new binary.
type ActivationCommittedError struct {
	Err error
}

func (e ActivationCommittedError) Error() string { return e.Err.Error() }

func (e ActivationCommittedError) Unwrap() error { return e.Err }

type resolverResponse struct {
	Data struct {
		URL       string `json:"url"`
		SHA256    string `json:"sha256"`
		Version   string `json:"version"`
		SizeBytes int64  `json:"size_bytes"`
		Size      int64  `json:"size"`
	} `json:"data"`
}

type artifact struct {
	URL     string
	SHA256  string
	Version string
	Size    int64
}

func New(opts Options) *Updater {
	if opts.HTTPClient == nil {
		opts.HTTPClient = http.DefaultClient
	}
	if opts.ExecutablePath == nil {
		opts.ExecutablePath = os.Executable
	}
	if opts.SmokeTimeout <= 0 {
		opts.SmokeTimeout = 5 * time.Second
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Updater{
		resolverURL:          strings.TrimSpace(opts.ResolverURL),
		resolverURLForRemote: opts.ResolverURLForRemote,
		currentVersion:       strings.TrimSpace(opts.CurrentVersion),
		stateDir:             opts.StateDir, httpClient: opts.HTTPClient,
		executablePath: opts.ExecutablePath, smokeTimeout: opts.SmokeTimeout, now: opts.Now,
	}
}

func (u *Updater) Stage(ctx context.Context, req Request) (Candidate, error) {
	if u == nil {
		return Candidate{}, errors.New("updater is not configured")
	}
	req.Version = strings.TrimSpace(req.Version)
	if req.Version == "" {
		return Candidate{}, errors.New("explicit target version is required")
	}
	resolverURL, err := u.resolveResolverURL(ctx, req.RemoteID)
	if err != nil {
		return Candidate{}, err
	}
	resolved, err := u.resolve(ctx, resolverURL, firstNonEmpty(req.Tag, DefaultTag))
	if err != nil {
		return Candidate{}, err
	}
	if resolved.Version != req.Version {
		return Candidate{}, fmt.Errorf("resolver version %q does not match requested %q", resolved.Version, req.Version)
	}
	if err := requireUpgrade(u.currentVersion, resolved.Version); err != nil {
		return Candidate{}, err
	}
	executablePath, err := u.executablePath()
	if err != nil {
		return Candidate{}, fmt.Errorf("resolve executable path: %w", err)
	}
	if resolvedPath, resolveErr := filepath.EvalSymlinks(executablePath); resolveErr == nil {
		executablePath = resolvedPath
	}
	info, err := os.Stat(executablePath)
	if err != nil {
		return Candidate{}, fmt.Errorf("stat executable: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(executablePath), ".paxd.update-*")
	if err != nil {
		return Candidate{}, fmt.Errorf("create staged executable: %w", err)
	}
	stagedPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(stagedPath)
		}
	}()
	gotSHA, gotSize, err := u.download(ctx, resolved, tmp)
	if err != nil {
		return Candidate{}, err
	}
	if !strings.EqualFold(gotSHA, resolved.SHA256) {
		return Candidate{}, fmt.Errorf("sha256 %s does not match expected %s", gotSHA, resolved.SHA256)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return Candidate{}, fmt.Errorf("chmod staged executable: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return Candidate{}, fmt.Errorf("sync staged executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Candidate{}, fmt.Errorf("close staged executable: %w", err)
	}
	if err := u.smoke(ctx, stagedPath, resolved.Version); err != nil {
		return Candidate{}, err
	}
	keep = true
	return Candidate{
		CommandID: req.CommandID, Path: stagedPath, ExecutablePath: executablePath,
		OldVersion: u.currentVersion, Version: resolved.Version, SHA256: gotSHA,
		Size: gotSize, OriginalFileMode: info.Mode().Perm(),
	}, nil
}

func (u *Updater) Activate(candidate Candidate, requestedBootID string) (ActivationRecord, error) {
	if u == nil {
		return ActivationRecord{}, errors.New("updater is not configured")
	}
	if candidate.Path == "" || candidate.ExecutablePath == "" || candidate.Version == "" {
		return ActivationRecord{}, errors.New("complete staged candidate is required")
	}
	stateDir := u.stateDir
	if strings.TrimSpace(stateDir) == "" {
		stateDir = filepath.Join(filepath.Dir(candidate.ExecutablePath), ".paxd-updates")
	}
	previousDir := filepath.Join(stateDir, "previous")
	if err := os.MkdirAll(previousDir, 0o700); err != nil {
		return ActivationRecord{}, fmt.Errorf("create previous binary directory: %w", err)
	}
	previousPath := filepath.Join(previousDir, "paxd-"+safeVersion(candidate.OldVersion))
	if err := copyFileAtomic(candidate.ExecutablePath, previousPath, candidate.OriginalFileMode); err != nil {
		return ActivationRecord{}, fmt.Errorf("preserve previous executable: %w", err)
	}
	record := ActivationRecord{
		CommandID: candidate.CommandID, RequestedBootID: requestedBootID,
		OldVersion: candidate.OldVersion, NewVersion: candidate.Version, SHA256: candidate.SHA256,
		ExecutablePath: candidate.ExecutablePath, PreviousPath: previousPath,
		ActivatedAt: u.now().UTC(),
	}
	if err := writeActivationRecord(stateDir, record, "prepared"); err != nil {
		return ActivationRecord{}, err
	}
	if err := os.Rename(candidate.Path, candidate.ExecutablePath); err != nil {
		return ActivationRecord{}, fmt.Errorf("activate staged executable: %w", err)
	}
	if err := syncPath(candidate.ExecutablePath); err != nil {
		return record, ActivationCommittedError{Err: fmt.Errorf("sync activated executable: %w", err)}
	}
	if err := syncDir(filepath.Dir(candidate.ExecutablePath)); err != nil {
		return record, ActivationCommittedError{Err: fmt.Errorf("sync executable directory: %w", err)}
	}
	if err := writeActivationRecord(stateDir, record, "activated"); err != nil {
		return record, ActivationCommittedError{Err: fmt.Errorf("persist activated record: %w", err)}
	}
	return record, nil
}

func (u *Updater) Cleanup(candidate Candidate) {
	if candidate.Path != "" {
		_ = os.Remove(candidate.Path)
	}
}

func (u *Updater) resolveResolverURL(ctx context.Context, remoteID string) (string, error) {
	if u.resolverURL != "" {
		return u.resolverURL, nil
	}
	if u.resolverURLForRemote == nil {
		return "", errors.New("update resolver is not configured")
	}
	resolverURL, err := u.resolverURLForRemote(ctx, strings.TrimSpace(remoteID))
	if err != nil {
		return "", fmt.Errorf("resolve update URL for remote %q: %w", remoteID, err)
	}
	resolverURL = strings.TrimSpace(resolverURL)
	if resolverURL == "" {
		return "", fmt.Errorf("update resolver for remote %q is empty", remoteID)
	}
	return resolverURL, nil
}

func (u *Updater) resolve(ctx context.Context, resolverURL string, tag string) (artifact, error) {
	endpoint, err := url.Parse(resolverURL)
	if err != nil {
		return artifact{}, safehttp.RedactError("parse paxd update resolver URL", err)
	}
	query := endpoint.Query()
	query.Set("platform", runtime.GOOS+"/"+runtime.GOARCH)
	query.Set("tags", tag)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil) // #nosec G107 -- resolver URL is local configuration, never command input.
	if err != nil {
		return artifact{}, safehttp.RedactError("create paxd update resolver request", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "paxd-updater")
	resp, err := safehttp.DoNoRedirect(u.httpClient, req)
	if err != nil {
		return artifact{}, safehttp.RedactError("request paxd update resolver", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return artifact{}, fmt.Errorf("resolver returned HTTP %d", resp.StatusCode)
	}
	var payload resolverResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return artifact{}, fmt.Errorf("decode resolver response: %w", err)
	}
	size := payload.Data.SizeBytes
	if size == 0 {
		size = payload.Data.Size
	}
	result := artifact{
		URL: normalizeArtifactURL(payload.Data.URL), SHA256: strings.TrimSpace(payload.Data.SHA256),
		Version: strings.TrimSpace(payload.Data.Version), Size: size,
	}
	if result.URL == "" || result.SHA256 == "" || result.Version == "" || result.Size <= 0 || result.Size > maxBinarySize {
		return artifact{}, errors.New("resolver returned incomplete or unsafe artifact metadata")
	}
	return result, nil
}

func ResolverURLFromCloudAPIURL(rawURL string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("parse cloud API URL: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return "", fmt.Errorf("cloud API URL must be an absolute HTTP(S) URL")
	}
	// The hosted tunnel is Access-protected; anonymous artifacts use the public origin.
	// Only map the known root origin, leaving self-hosted paths and ports intact.
	if base.Scheme == "https" && strings.EqualFold(base.Host, "wsapi.lakeward.net") &&
		strings.TrimRight(base.Path, "/") == "" && base.User == nil {
		base.Host = "api.lakeward.net"
	}
	base.Path = strings.TrimRight(base.Path, "/") + resolverPath
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func (u *Updater) download(ctx context.Context, item artifact, target *os.File) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.URL, nil) // #nosec G107 -- URL is supplied by the configured trusted resolver.
	if err != nil {
		return "", 0, safehttp.RedactError("create paxd update download request", err)
	}
	req.Header.Set("User-Agent", "paxd-updater")
	resp, err := safehttp.DoNoRedirect(u.httpClient, req)
	if err != nil {
		return "", 0, safehttp.RedactError("download paxd update", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(target, hash), io.LimitReader(resp.Body, item.Size+1))
	if err != nil {
		return "", 0, fmt.Errorf("write staged executable: %w", err)
	}
	if written != item.Size {
		return "", 0, fmt.Errorf("download size %d does not match expected %d", written, item.Size)
	}
	return hex.EncodeToString(hash.Sum(nil)), written, nil
}

func (u *Updater) smoke(parent context.Context, path, version string) error {
	ctx, cancel := context.WithTimeout(parent, u.smokeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, path, "--version").CombinedOutput() // #nosec G204 -- path is a verified staged executable in the installed executable directory.
	if err != nil {
		return fmt.Errorf("smoke test staged executable: %w", err)
	}
	fields := strings.Fields(string(output))
	for _, field := range fields {
		if strings.TrimPrefix(field, "v") == strings.TrimPrefix(version, "v") {
			return nil
		}
	}
	return fmt.Errorf("staged executable version output %q does not contain target %q", strings.TrimSpace(string(output)), version)
}

func requireUpgrade(current, target string) error {
	currentParts, currentOK := parseVersion(current)
	targetParts, targetOK := parseVersion(target)
	if !targetOK {
		return fmt.Errorf("target version %q is not semantic", target)
	}
	if !currentOK {
		return nil
	}
	for i := range currentParts {
		if targetParts[i] > currentParts[i] {
			return nil
		}
		if targetParts[i] < currentParts[i] {
			return fmt.Errorf("refusing downgrade from %s to %s", current, target)
		}
	}
	return fmt.Errorf("target version %s is already running", target)
}

func parseVersion(raw string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(raw), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return out, false
		}
		out[i] = value
	}
	return out, true
}

func copyFileAtomic(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".previous-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode.Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, destination)
}

func writeActivationRecord(stateDir string, record ActivationRecord, phase string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create update state directory: %w", err)
	}
	payload := struct {
		ActivationRecord
		Phase string `json:"phase"`
	}{ActivationRecord: record, Phase: phase}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".activation-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(stateDir, "activation.json")); err != nil {
		return err
	}
	return syncDir(stateDir)
}

func syncPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func normalizeArtifactURL(raw string) string {
	if !strings.HasPrefix(raw, "gs://") {
		return strings.TrimSpace(raw)
	}
	rest := strings.TrimPrefix(raw, "gs://")
	bucket, object, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" || object == "" {
		return ""
	}
	return "https://storage.googleapis.com/" + bucket + "/" + object
}

func safeVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown"
	}
	return strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(raw)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
