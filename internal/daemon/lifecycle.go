package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

const defaultRemoteRestartGrace = 10 * time.Second

// ExitRequest asks main to enter the same bounded shutdown path used for OS
// termination signals. Receipt means the originating command ACK was delivered.
type ExitRequest struct {
	CommandID     string
	Reason        string
	ShutdownGrace time.Duration
}

func (c *lifecycleCoordinator) ExitRequests() <-chan ExitRequest {
	if c == nil {
		return nil
	}
	return c.exit
}

func newBootID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "boot_" + hex.EncodeToString(raw[:]), nil
}

func (r *Runtime) ExitRequests() <-chan ExitRequest {
	if r == nil || r.maintenance == nil {
		return nil
	}
	return r.maintenance.ExitRequests()
}
