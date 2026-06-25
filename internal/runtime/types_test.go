package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExitClassValid(t *testing.T) {
	tests := []struct {
		class ExitClass
		want  bool
	}{
		{class: ExitTransient, want: true},
		{class: ExitAuth, want: true},
		{class: ExitConfig, want: true},
		{class: ExitTerminal, want: true},
		{class: ExitClass("unknown")},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.class.Valid(), "%q Valid()", tt.class)
	}
}

func TestExitConstructors(t *testing.T) {
	tests := []struct {
		name string
		got  Exit
		want ExitClass
	}{
		{name: "transient", got: TransientExit("network", "network error"), want: ExitTransient},
		{name: "auth", got: AuthExit("unauthorized", "unauthorized"), want: ExitAuth},
		{name: "config", got: ConfigExit("missing_command", "missing command"), want: ExitConfig},
		{name: "terminal", got: TerminalExit("stopped", "stopped"), want: ExitTerminal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got.Class)
			assert.NotEmpty(t, tt.got.Code)
			assert.NotEmpty(t, tt.got.Message)
		})
	}
}

func TestCanceledExitUsesContextError(t *testing.T) {
	exit := CanceledExit(context.Canceled)
	assert.Equal(t, ExitTerminal, exit.Class)
	assert.Equal(t, "canceled", exit.Code)
	assert.Equal(t, context.Canceled.Error(), exit.Message)

	custom := CanceledExit(errors.New("slot stopped"))
	assert.Equal(t, "slot stopped", custom.Message)
}

func TestExitWithDetailCopiesIntoMap(t *testing.T) {
	exit := TransientExit("network", "network error").WithDetail("remote_id", "remote_prod")
	require.NotNil(t, exit.Details)
	assert.Equal(t, "remote_prod", exit.Details["remote_id"])
}

func TestHeartbeatConfigDefaults(t *testing.T) {
	got := (HeartbeatConfig{}).WithDefaults()
	assert.Equal(t, 15*time.Second, got.PingInterval)
	assert.Equal(t, 45*time.Second, got.ReadTimeout)

	custom := (HeartbeatConfig{PingInterval: time.Second, ReadTimeout: 2 * time.Second}).WithDefaults()
	assert.Equal(t, time.Second, custom.PingInterval)
	assert.Equal(t, 2*time.Second, custom.ReadTimeout)
}
