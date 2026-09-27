package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/pax-beehive/paxd/internal/remotelogin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenSetupRegistersPersistsStartsAndDoesNotPair(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PAX_REGISTRATION_TOKEN", "one-time-secret")
	registrations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registrations++
		assert.Equal(t, "/api/v1/node/register", r.URL.Path)
		assert.Equal(t, "one-time-secret", r.Header.Get("X-Registration-Token"))
		fmt.Fprint(w, `{"code":200,"data":{"node_id":"node_quick","api_key":"node-key-secret"}}`)
	}))
	defer server.Close()
	defer stubRemoteLogin(t, func(context.Context, remotelogin.LoginSpec, remotelogin.Options) (remotelogin.LoginResult, error) {
		t.Fatal("token setup must not request browser pairing")
		return remotelogin.LoginResult{}, nil
	})()
	var actions []string
	defer stubServiceOps(t, func(serviceInstallOptions) error {
		assert.Empty(t, os.Getenv("PAX_REGISTRATION_TOKEN"))
		actions = append(actions, "install")
		return nil
	}, func(action string, _ bool) error { actions = append(actions, action); return nil })()
	defer stubVerifyLocalAPI(t, func(context.Context, time.Duration) error { actions = append(actions, "verify"); return nil })()
	app := newApp()
	var output bytes.Buffer
	app.Writer = &output
	args := []string{"paxd", "setup", "--cloud-url", server.URL, "--registration-token-env"}
	require.NoError(t, app.Run(context.Background(), args))
	assert.Equal(t, []string{"install", "restart", "verify"}, actions)
	assert.Equal(t, 1, registrations)
	assert.Contains(t, output.String(), "Connected default remote as node_quick")
	assert.NotContains(t, output.String(), "secret")
	keyPath := filepath.Join(home, ".paxd", "secrets", "remotes", "default", "node_key")
	key, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, "node-key-secret\n", string(key))
	stat, err := os.Stat(keyPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), stat.Mode().Perm())

	// Re-running a pasted command cannot consume another token or replace ownership.
	t.Setenv("PAX_REGISTRATION_TOKEN", "another-account-token")
	err = newApp().Run(context.Background(), args)
	require.ErrorContains(t, err, "already configured")
	assert.Equal(t, 1, registrations)
	assert.Len(t, actions, 3)
	key, err = os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, "node-key-secret\n", string(key))
}

type tokenClientFunc func(*cloud.RegisterNodeRequest, string) (*cloud.RegisterNodeResponse, error)

func (fn tokenClientFunc) RegisterNode(req *cloud.RegisterNodeRequest, token string) (*cloud.RegisterNodeResponse, error) {
	return fn(req, token)
}

func TestTokenSetupRejectsMissingExpiredAndIncompleteCredentials(t *testing.T) {
	for _, test := range []struct {
		name     string
		token    string
		response *cloud.RegisterNodeResponse
		err      error
		want     string
	}{
		{name: "missing", want: "PAX_REGISTRATION_TOKEN is required"},
		{name: "expired", token: "secret", err: fmt.Errorf("expired secret"), want: "generate a fresh command"},
		{name: "incomplete", token: "secret", response: &cloud.RegisterNodeResponse{NodeID: "node_1"}, want: "did not return node credentials"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("PAX_REGISTRATION_TOKEN", test.token)
			require.NoError(t, ensurePaxdHome())
			cfg, err := loadRuntimeConfig()
			require.NoError(t, err)
			_, err = registerSetupToken(context.Background(), cfg, remotelogin.LoginSpec{RemoteID: "default"}, tokenClientFunc(func(_ *cloud.RegisterNodeRequest, token string) (*cloud.RegisterNodeResponse, error) {
				assert.Equal(t, test.token, token)
				return test.response, test.err
			}))
			require.ErrorContains(t, err, test.want)
			assert.NotContains(t, err.Error(), "secret")
			assert.Empty(t, os.Getenv("PAX_REGISTRATION_TOKEN"))
		})
	}
}
