//go:build !windows

package harnessauth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/require"
)

func TestCancellationStopsLoginDescendants(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "descendant-completed")
	script := fmt.Sprintf("(sleep 0.5; touch '%s') >/dev/null 2>&1 &\nprintf '%%s\\n' '%s'\nprintf 'Paste code here > '\nwait\n", marker, testURL)
	m := fakeCLI(t, script, time.Minute)
	view := startLogin(t, m)
	awaitState(t, m, view.SessionID, "awaiting_code")
	_, err := m.Login(context.Background(), testOwner, "cancel", control.HarnessAuthLoginCommand{Harness: "claude", Operation: "cancel", SessionID: view.SessionID})
	require.NoError(t, err)
	m.Close()
	require.Never(t, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, time.Second, 10*time.Millisecond, "A cancelled login descendant must not complete its work.")
}
