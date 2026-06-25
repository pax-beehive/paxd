//go:build windows

package runtime

import "os"

func execInterruptSignal() os.Signal {
	return os.Interrupt
}
