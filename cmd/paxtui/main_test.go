package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunRendersLocalOverviewFromClient(t *testing.T) {
	restore := replaceOverviewClient(fakeOverviewClient{
		value: &control.LocalOverview{
			Harnesses: []control.HarnessView{{Harness: "codex", State: "available"}},
			Sessions:  []control.LocalSessionView{{ID: "codex:sess_1"}},
		},
	})
	defer restore()
	var stdout bytes.Buffer

	err := run(context.Background(), []string{"--debug-http", "http://127.0.0.1:1"}, &stdout)

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "Pax Local Overview")
	assert.Contains(t, stdout.String(), "codex:sess_1")
}

func TestRunPassesSocketFlagToClientFactory(t *testing.T) {
	original := newOverviewClient
	var gotSocket string
	var gotDebugHTTP string
	newOverviewClient = func(socket string, debugHTTP string) overviewClient {
		gotSocket = socket
		gotDebugHTTP = debugHTTP
		return fakeOverviewClient{value: &control.LocalOverview{}}
	}
	defer func() { newOverviewClient = original }()

	err := run(context.Background(), []string{"--socket", "/tmp/paxd.sock"}, &bytes.Buffer{})

	require.NoError(t, err)
	assert.Equal(t, "/tmp/paxd.sock", gotSocket)
	assert.Equal(t, "", gotDebugHTTP)
}

func TestRunRejectsUnknownArgument(t *testing.T) {
	err := run(context.Background(), []string{"--wat"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "flag provided but not defined")
}

func TestRunRequiresFlagValues(t *testing.T) {
	for _, args := range [][]string{{"--socket"}, {"--debug-http"}} {
		err := run(context.Background(), args, &bytes.Buffer{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "flag needs an argument")
	}
}

func TestRenderNilOverview(t *testing.T) {
	var stdout bytes.Buffer

	err := renderOverview(&stdout, nil)

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "No local overview")
}

func TestExpandHomeLeavesPlainPath(t *testing.T) {
	assert.Equal(t, "/tmp/paxd.sock", expandHome("/tmp/paxd.sock"))
}

func replaceOverviewClient(client fakeOverviewClient) func() {
	original := newOverviewClient
	newOverviewClient = func(string, string) overviewClient { return client }
	return func() { newOverviewClient = original }
}

type fakeOverviewClient struct {
	value *control.LocalOverview
}

func (c fakeOverviewClient) GetLocalOverview(context.Context) (control.QueryResult, error) {
	return control.QueryResult{LocalOverview: c.value}, nil
}
