//go:build !windows

package paxlinstall

import (
	"fmt"
	"golang.org/x/sys/unix"
	"os"
)

func lockExecutable(path string) (func(), error) {
	file, err := os.OpenFile(path+".update.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open paxl upgrade lock: %w", err)
	}
	if err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("paxl upgrade is busy: %w", err)
	}
	return func() { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN); _ = file.Close() }, nil
}
