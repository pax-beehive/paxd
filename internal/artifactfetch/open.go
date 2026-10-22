package artifactfetch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

// Result is the outcome of a check. Policy and file-state problems are
// denials; only internal failures are returned as errors.
type Result struct {
	Allowed   bool
	Path      string
	Filename  string
	SizeBytes int64
	Denial    *Denial
}

// Opened is a fetchable file opened through a home-rooted handle. The caller
// owns File and must close it.
type Opened struct {
	Target
	File *os.File
	Size int64
}

// Check runs every rule except the content scan. It opens the file to fstat
// the handle but never reads its content.
func Check(home, rawPath string) (Result, error) {
	opened, denial, err := Open(home, rawPath)
	if err != nil {
		return Result{}, err
	}
	result := Result{Path: displayPath(rawPath), Filename: displayFilename(rawPath)}
	if denial != nil {
		result.Denial = denial
		return result, nil
	}
	_ = opened.File.Close()
	result.Allowed = true
	result.Path = opened.Path
	result.Filename = opened.Filename
	result.SizeBytes = opened.Size
	return result, nil
}

// Open applies the path rules, opens the file through os.Root at home so a
// symlink cannot escape, and applies the stat rules to the opened handle.
func Open(home, rawPath string) (*Opened, *Denial, error) {
	target, denial := CheckPath(home, rawPath)
	if denial != nil {
		return nil, denial, nil
	}
	root, err := os.OpenRoot(filepath.Clean(home))
	if err != nil {
		return nil, nil, fmt.Errorf("open home root: %w", err)
	}
	defer root.Close()
	// Reject directories, devices and FIFOs before opening them at all; the
	// fstat below remains the authoritative check on the opened handle.
	if info, statErr := root.Stat(target.Rel); statErr != nil {
		return nil, openDenial(home, target.Path, statErr), nil
	} else if !info.Mode().IsRegular() {
		return nil, CheckInfo(target.Path, info), nil
	}
	resolved, denial := checkResolvedPath(home, target.Path)
	if denial != nil {
		return nil, denial, nil
	}
	file, err := root.OpenFile(target.Rel, openFlags, 0)
	if err != nil {
		return nil, openDenial(home, target.Path, err), nil
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("stat fetch source: %w", err)
	}
	// The link could be swapped between resolving and opening; the opened
	// handle must be the file whose resolved path passed the rules.
	if resolvedInfo, statErr := os.Stat(resolved); statErr != nil || !os.SameFile(info, resolvedInfo) {
		_ = file.Close()
		return nil, &Denial{
			Code: CodeNotFound, Rule: RuleHomeRoot,
			Message: "This file changed while it was being opened. Try again.",
		}, nil
	}
	if denial := CheckInfo(target.Path, info); denial != nil {
		_ = file.Close()
		return nil, denial, nil
	}
	return &Opened{Target: target, File: file, Size: info.Size()}, nil, nil
}

// checkResolvedPath re-runs the path rules on the symlink-resolved path.
// os.Root follows relative links that stay inside home, so a link such as
// proj/notes.md -> ../.env would otherwise pass the rules under its own name.
// Home is resolved too, because it may sit under a symlinked ancestor such as
// /var -> /private/var on macOS.
func checkResolvedPath(home, cleanedPath string) (string, *Denial) {
	resolved, err := filepath.EvalSymlinks(cleanedPath)
	if err != nil {
		return "", openDenial(home, cleanedPath, err)
	}
	realHome, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil {
		return "", outsideHome()
	}
	if _, denial := CheckPath(realHome, resolved); denial != nil {
		return "", denial
	}
	return resolved, nil
}

// CheckInfo applies the stat-based rules (regular file, owner-only, size) to
// the FileInfo of an already opened handle.
func CheckInfo(cleanedPath string, info fs.FileInfo) *Denial {
	return checkInfo(cleanedPath, info, runtime.GOOS != "windows")
}

func checkInfo(cleanedPath string, info fs.FileInfo, ownerOnlyRule bool) *Denial {
	if !info.Mode().IsRegular() {
		return &Denial{
			Code: CodeNotRegularFile, Rule: RuleHomeRoot,
			Message: "Only regular files can be fetched.",
		}
	}
	if ownerOnlyRule && info.Mode().Perm()&0o077 == 0 {
		return &Denial{
			Code: CodeOwnerOnlyDenied, Rule: RuleOwnerOnly,
			Message: ownerOnlyMessage(cleanedPath),
		}
	}
	if info.Size() > MaxSizeBytes {
		return &Denial{
			Code: CodeTooLarge, Rule: RuleSizeLimit,
			Message: "Files larger than 5 GiB cannot be fetched.",
		}
	}
	return nil
}

func openDenial(home, cleanedPath string, err error) *Denial {
	if isEscapeError(err) || escapesHome(home, cleanedPath) {
		return outsideHome()
	}
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return &Denial{Code: CodeNotFound, Rule: RuleHomeRoot, Message: "This file no longer exists."}
	case errors.Is(err, fs.ErrPermission):
		return &Denial{
			Code: CodePermissionDenied, Rule: RuleHomeRoot,
			Message: "paxd does not have permission to read this file.",
		}
	default:
		return &Denial{
			Code: CodeNotRegularFile, Rule: RuleHomeRoot,
			Message: "This file cannot be opened as a regular file.",
		}
	}
}

// isEscapeError recognizes os.Root's unexported "path escapes from parent"
// error, which is returned when a symlink or ".." leaves the root.
func isEscapeError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "path escapes from parent")
}

// escapesHome is a fallback for platforms that report escapes differently:
// resolve the path and check it lexically against the resolved home.
func escapesHome(home, cleanedPath string) bool {
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(cleanedPath)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(resolvedHome, resolved)
	return err != nil || !filepath.IsLocal(rel)
}

func displayPath(rawPath string) string {
	if filepath.IsAbs(rawPath) && !strings.ContainsAny(rawPath, "\x00\r\n") {
		return filepath.Clean(rawPath)
	}
	return rawPath
}

func displayFilename(rawPath string) string {
	if rawPath == "" {
		return ""
	}
	return filepath.Base(displayPath(rawPath))
}
