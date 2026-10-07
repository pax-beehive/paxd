package paxlinstall

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/pax-beehive/paxd/internal/updater"
)

type Installer struct{ Options updater.Options }

// Upgrade uses the exact same entry point as probing and session discovery.
// Existing processes keep their open executable; new processes see the atomic rename.
func (i *Installer) Upgrade(ctx context.Context, commandID, remoteID, version, tag string, phase func(string)) (Observation, error) {
	before := Probe(ctx)
	if before.Status != "installed" {
		return before, fmt.Errorf("paxl is not manageable: %s", before.Error)
	}
	if err := requireManagedPath(before.Path); err != nil {
		return before, err
	}
	lockedPath := before.Path
	unlock, err := lockExecutable(lockedPath)
	if err != nil {
		return before, err
	}
	defer unlock()
	before = Probe(ctx)
	if before.Status != "installed" || before.Path != lockedPath {
		return before, errors.New("paxl changed while acquiring upgrade lock")
	}
	if sameVersion(before.Version, version) {
		return before, nil
	}
	original, err := os.ReadFile(before.Path)
	if err != nil {
		return before, err
	}
	fingerprint := sha256.Sum256(original)
	options := i.Options
	options.Product = "paxl"
	options.CurrentVersion = before.Version
	options.ExecutablePath = func() (string, error) { return before.Path, nil }
	options.VerifyExecutable = verifyVersion
	update := updater.New(options)
	phase("downloading")
	candidate, err := update.Stage(ctx, updater.Request{CommandID: commandID, RemoteID: remoteID, Version: version, Tag: tag})
	if err != nil {
		return before, err
	}
	defer update.Cleanup(candidate)
	current := Probe(ctx)
	content, err := os.ReadFile(before.Path)
	if err != nil {
		return before, err
	}
	if current.Path != before.Path || sha256.Sum256(content) != fingerprint {
		return current, errors.New("paxl executable changed during upgrade")
	}
	phase("activating")
	record, err := update.Activate(candidate, "")
	if err != nil {
		var committed updater.ActivationCommittedError
		if errors.As(err, &committed) {
			err = errors.Join(err, update.Restore(record))
		}
		return before, err
	}
	phase("verifying")
	after := Probe(ctx)
	if after.Status != "installed" || after.Path != before.Path || !sameVersion(after.Version, version) {
		return after, errors.Join(errors.New("installed paxl did not report the target version"), update.Restore(record))
	}
	return after, nil
}

func verifyVersion(ctx context.Context, path, version string) error {
	got, err := versionAt(ctx, path)
	if err != nil {
		return err
	}
	if !sameVersion(got.Version, version) {
		return fmt.Errorf("paxl version %q does not match target %q", got.Version, version)
	}
	return nil
}
func sameVersion(a, b string) bool { return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v") }

func requireManagedPath(path string) error {
	for _, marker := range []string{"/Cellar/", "/Caskroom/", "/nix/store/", "/.asdf/installs/", "/mise/installs/"} {
		if strings.Contains(path, marker) {
			return errors.New("paxl is managed by a package manager; use that manager to upgrade it")
		}
	}
	return nil
}
