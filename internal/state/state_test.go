package state

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMachineTransitionsFromStartingDirectlyToRunning(t *testing.T) {
	machine := NewMachine()

	err := machine.Transition(RUNNING)

	require.NoError(t, err)
	assert.Equal(t, RUNNING, machine.State())
}

func TestMachineStopsFromStartingWithoutRegistration(t *testing.T) {
	machine := NewMachine()

	err := machine.Transition(STOPPING)

	require.NoError(t, err)
	assert.Equal(t, STOPPING, machine.State())
	assert.ErrorIs(t, machine.Context().Err(), context.Canceled)
}
