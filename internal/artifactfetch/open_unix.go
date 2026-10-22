//go:build unix

package artifactfetch

import (
	"os"
	"syscall"
)

// O_NONBLOCK keeps a file swapped to a FIFO between stat and open from
// blocking the single in-flight node-control request; it has no effect on
// regular files, and fstat of the handle then rejects anything irregular.
const openFlags = os.O_RDONLY | syscall.O_NONBLOCK
