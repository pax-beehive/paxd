//go:build !windows

package runtime

import (
	"os"
	"syscall"
)

func execInterruptSignal() os.Signal {
	return syscall.SIGTERM
}
