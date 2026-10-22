//go:build !unix

package artifactfetch

import "os"

const openFlags = os.O_RDONLY
