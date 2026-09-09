package secretchannel

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// fileRefPrefix mirrors the convention already used by
// internal/remotesecrets for on-disk secret references.
const fileRefPrefix = "file:"

// FileDrop writes a decrypted secret to a single-use file under Dir and
// deletes it again once TTL has passed. It never overwrites an existing
// path and never follows a symlink placed at the target name: each write
// picks a fresh random filename and creates it with O_EXCL, so a pre-existing
// path (symlink or otherwise) at that exact name is treated as a hard error
// rather than being written through.
//
// This directory holds hand-off files only: something on the machine (an
// agent, a script) is expected to read the file promptly and delete it. TTL
// and Sweep are the safety net for whatever doesn't get read in time, not
// the primary cleanup mechanism.
type FileDrop struct {
	Dir string
	TTL time.Duration
	Now func() time.Time
}

func (d FileDrop) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Write persists plaintext verbatim: no trimming, no trailing newline. The
// consumer is a program, not a human at a terminal.
func (d FileDrop) Write(plaintext []byte) (fileRef string, expiresAt time.Time, err error) {
	if err := os.MkdirAll(d.Dir, 0700); err != nil {
		return "", time.Time{}, fmt.Errorf("secretchannel: create transient dir: %w", err)
	}
	name, err := randomID(rand.Reader)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secretchannel: generate file name: %w", err)
	}
	path := filepath.Join(d.Dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secretchannel: create transient file: %w", err)
	}
	if _, writeErr := f.Write(plaintext); writeErr != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", time.Time{}, fmt.Errorf("secretchannel: write transient file: %w", writeErr)
	}
	if closeErr := f.Close(); closeErr != nil {
		_ = os.Remove(path)
		return "", time.Time{}, fmt.Errorf("secretchannel: close transient file: %w", closeErr)
	}
	// Stamp mtime from our own clock (equal to the OS clock in production)
	// rather than trusting whatever the OS assigned, so Sweep's age
	// comparisons stay consistent with an injected clock in tests.
	writtenAt := d.now()
	if err := os.Chtimes(path, writtenAt, writtenAt); err != nil {
		_ = os.Remove(path)
		return "", time.Time{}, fmt.Errorf("secretchannel: stamp transient file: %w", err)
	}
	return fileRefPrefix + path, writtenAt.Add(d.TTL), nil
}

// Sweep removes files older than TTL. It is safe to call on a directory
// that does not exist yet (nothing has been written) or that contains
// entries this package did not create (they are left alone only if they
// are not regular files; regular files are treated as ours, since Dir is a
// dedicated directory).
func (d FileDrop) Sweep() (int, error) {
	return d.cleanup(func(age time.Duration) bool { return age > d.TTL })
}

// CleanupStartup removes every file in Dir unconditionally. It must only be
// called once, at paxd startup, before any channel has been opened: no
// in-memory Registry state survives a restart, so any file left over from a
// previous run can never be legitimately claimed again.
func (d FileDrop) CleanupStartup() (int, error) {
	return d.cleanup(func(time.Duration) bool { return true })
}

func (d FileDrop) cleanup(shouldRemove func(age time.Duration) bool) (int, error) {
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("secretchannel: list transient dir: %w", err)
	}
	now := d.now()
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if shouldRemove(now.Sub(info.ModTime())) {
			if err := os.Remove(filepath.Join(d.Dir, entry.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}
