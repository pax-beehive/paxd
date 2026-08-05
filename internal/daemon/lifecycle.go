package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
)

const defaultRemoteRestartGrace = 30 * time.Second

// ExitRequest asks main to enter the same bounded shutdown path used for OS
// termination signals. Receipt means the originating command ACK was delivered.
type ExitRequest struct {
	CommandID     string
	Reason        string
	ShutdownGrace time.Duration
}

type lifecycleCoordinator struct {
	mu        sync.Mutex
	bootID    string
	pending   map[string]ExitRequest
	committed bool
	exit      chan ExitRequest
}

func newLifecycleCoordinator(bootID string) *lifecycleCoordinator {
	return &lifecycleCoordinator{
		bootID:  bootID,
		pending: make(map[string]ExitRequest),
		exit:    make(chan ExitRequest, 1),
	}
}

func (c *lifecycleCoordinator) BootID() string {
	if c == nil {
		return ""
	}
	return c.bootID
}

func (c *lifecycleCoordinator) ScheduleRestart(commandID string, command control.RestartPaxdCommand) {
	if c == nil || commandID == "" {
		return
	}
	grace := defaultRemoteRestartGrace
	if command.ShutdownGraceSeconds > 0 {
		grace = time.Duration(command.ShutdownGraceSeconds) * time.Second
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.committed {
		return
	}
	c.pending[commandID] = ExitRequest{
		CommandID:     commandID,
		Reason:        command.Reason,
		ShutdownGrace: grace,
	}
}

func (c *lifecycleCoordinator) ConfirmAckDelivered(commandID string) {
	if c == nil || commandID == "" {
		return
	}
	c.mu.Lock()
	request, ok := c.pending[commandID]
	if !ok || c.committed {
		c.mu.Unlock()
		return
	}
	c.committed = true
	c.mu.Unlock()
	c.exit <- request
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
