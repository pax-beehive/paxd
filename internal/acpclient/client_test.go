package acpclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionListerTimesOutWhenACPDoesNotRespond(t *testing.T) {
	started := time.Now()
	lister := SessionLister{
		Command: []string{"sh", "-c", "cat >/dev/null"},
		Timeout: 100 * time.Millisecond,
	}
	_, err := lister.List(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List() error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("List() took %s, want fast timeout", elapsed)
	}
}
