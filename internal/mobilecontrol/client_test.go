package mobilecontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testSession = "m-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
const testRecording = "11111111-2222-3333-4444-555555555555"
const testFrame = "aaaaaaaa-1111-2222-3333-bbbbbbbbbbbb"
const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fixture(t *testing.T, handler http.HandlerFunc) (*Client, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(server.URL, "http://"))
	require.NoError(t, err)
	root := t.TempDir()
	require.NoError(t, os.Chmod(root, 0700))
	require.NoError(t, os.Mkdir(filepath.Join(root, "secrets"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secrets", "admin-token"), []byte(testToken+"\n"), 0600))
	configPath := filepath.Join(root, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(`{"dataDir":%q,"controlPort":%s}`, root, portText)), 0600))
	return &Client{ConfigPath: configPath}, root
}

func raw(value string) json.RawMessage { return json.RawMessage(value) }

func publicDevice() string {
	return fmt.Sprintf(`{"id":%q,"deviceId":%q,"template":"clean","state":"running","takeover":true,"createdAt":"2026-11-10T00:00:00Z","credentialFile":"/secret/session.json","token":"must-not-escape","capabilities":{"screen":true,"internalAudioRecording":true,"realtimeMedia":true,"hostNetworkIsolation":false}}`, testSession, testSession)
}

func TestStateAndCreateStripCredentialsAndNeverAdvertiseRemoteWebRTC(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer "+testToken, r.Header.Get("Authorization"))
		require.Equal(t, "remote:owner", r.Header.Get("X-Pax-Mobile-Actor"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/admin/templates":
			fmt.Fprint(w, `[{"name":"clean","available":true,"path":"/private/template"}]`)
		case "/v1/admin/sessions":
			if r.Method == http.MethodPost {
				require.NoError(t, json.NewDecoder(r.Body).Decode(&map[string]any{}))
				fmt.Fprint(w, publicDevice())
				return
			}
			fmt.Fprint(w, "["+publicDevice()+"]")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	state, err := client.Request(context.Background(), "remote:owner", "state", nil)
	require.NoError(t, err)
	require.NotContains(t, string(state), "must-not-escape")
	require.NotContains(t, string(state), "credentialFile")
	require.NotContains(t, string(state), "/private")
	require.Contains(t, string(state), `"remoteWebRTC":false`)
	require.Contains(t, string(state), `"realtimeMedia":false`)
	created, err := client.Request(context.Background(), "remote:owner", "create", raw(`{"template":"clean"}`))
	require.NoError(t, err)
	require.NotContains(t, string(created), "credentialFile")
	require.NotContains(t, string(created), "must-not-escape")
}

func TestMissingRuntimeProducesSafeUnavailableState(t *testing.T) {
	client := &Client{ConfigPath: filepath.Join(t.TempDir(), "not-installed.json")}
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	state, err := client.Request(context.Background(), "remote:owner", "state", nil)
	require.NoError(t, err)
	require.JSONEq(t, `{"available":false,"mode":"screenshots-and-recordings","templates":[],"sessions":[],"remoteWebRTC":false}`, string(state))
	_, err = client.Request(context.Background(), "remote:owner", "create", raw(`{"template":"clean"}`))
	require.Error(t, err)
	require.NotContains(t, err.Error(), client.ConfigPath)
}

func TestDeniedPayloadsNeverReachRuntime(t *testing.T) {
	var requests atomic.Int64
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); fmt.Fprint(w, `{}`) })
	tests := []struct{ operation, payload string }{
		{"live_open", `{}`}, {"live", `{}`}, {"install_app", `{"apkPath":"/host"}`}, {"proxy", `{"url":"http://127.0.0.1"}`},
		{"state", `{"token":"x"}`}, {"templates", `{"port":7342}`}, {"create", `{"template":"clean","key":"arbitrary"}`},
		{"start", `{"sessionId":"../../secrets"}`}, {"stop", `{"sessionId":"` + testSession + `","url":"https://attacker"}`},
		{"tap", `{"sessionId":"` + testSession + `","x":1,"y":2}`}, {"tap", `{"sessionId":"` + testSession + `","frameId":"` + testFrame + `","x":-1,"y":2}`},
		{"text", `{"sessionId":"` + testSession + `","frameId":"` + testFrame + `","text":"a%s"}`},
		{"key", `{"sessionId":"` + testSession + `","frameId":"` + testFrame + `","key":"POWER"}`},
		{"recording_read", `{"sessionId":"` + testSession + `","recordingId":"` + testRecording + `","offset":-1}`},
		{"recording_read", `{"sessionId":"` + testSession + `","recordingId":"` + testRecording + `","length":262145}`},
		{"recording_read", `{"sessionId":"` + testSession + `","recordingId":"` + testRecording + `","path":"/host/file"}`},
		{"state", `null`}, {"state", `[]`}, {"state", `{"unknown":"` + strings.Repeat("x", 16384) + `"}`},
	}
	for _, test := range tests {
		t.Run(test.operation+test.payload[:min(20, len(test.payload))], func(t *testing.T) {
			_, err := client.Request(context.Background(), "remote:owner", test.operation, raw(test.payload))
			require.Error(t, err)
		})
	}
	require.Equal(t, int64(0), requests.Load())
}

func TestInputUsesExactSessionActionAndFrameWithNoRetry(t *testing.T) {
	var requests atomic.Int64
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		require.Equal(t, "/v1/admin/sessions/"+testSession+"/action", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "tap", body["action"])
		require.Equal(t, testFrame, body["args"].(map[string]any)["frameId"])
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"error":{"message":"/private/path and Bearer token"}}`)
	})
	_, err := client.Request(context.Background(), "remote:owner", "tap", raw(`{"sessionId":"`+testSession+`","frameId":"`+testFrame+`","x":10,"y":20}`))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "/private")
	require.NotContains(t, err.Error(), "Bearer")
	require.Equal(t, int64(1), requests.Load())
}

func TestRecordingReadUsesBoundedRangeAndValidatesResponse(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/admin/sessions/"+testSession+"/artifacts/"+testRecording, r.URL.Path)
		require.Equal(t, "bytes=2-5", r.Header.Get("Range"))
		w.Header().Set("Content-Type", "audio/ogg")
		w.Header().Set("Content-Range", "bytes 2-5/6")
		w.WriteHeader(http.StatusPartialContent)
		fmt.Fprint(w, "tone")
	})
	result, err := client.Request(context.Background(), "remote:owner", "recording_read", raw(`{"sessionId":"`+testSession+`","recordingId":"`+testRecording+`","offset":2,"length":4}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"`+testRecording+`","mime":"audio/ogg","data":"dG9uZQ==","offset":2,"length":4,"totalBytes":6,"eof":true}`, string(result))
	for _, test := range []struct {
		status                   int
		contentRange, mime, body string
	}{
		{200, "bytes 2-5/6", "audio/ogg", "tone"},
		{206, "bytes 0-5/6", "audio/ogg", "tone"},
		{206, "bytes 2-5/6", "text/html", "tone"},
		{206, "bytes 2-5/6", "audio/ogg", "short"},
		{206, "bytes 2-262146/1000000", "audio/ogg", "tone"},
	} {
		t.Run(fmt.Sprint(test), func(t *testing.T) {
			bad, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.mime)
				w.Header().Set("Content-Range", test.contentRange)
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			})
			_, err := bad.Request(context.Background(), "remote:owner", "recording_read", raw(`{"sessionId":"`+testSession+`","recordingId":"`+testRecording+`","offset":2,"length":4}`))
			require.Error(t, err)
		})
	}
}

func TestCredentialsMustBePrivateRegularFiles(t *testing.T) {
	client, root := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach runtime") })
	filename := filepath.Join(root, "secrets", "admin-token")
	require.NoError(t, os.Chmod(filename, 0644))
	_, err := client.resolve()
	require.Error(t, err)
	require.NoError(t, os.Chmod(filename, 0600))
	_, err = client.resolve()
	require.NoError(t, err)
	require.NoError(t, os.Rename(filename, filename+".old"))
	require.NoError(t, os.Symlink(filename+".old", filename))
	_, err = client.resolve()
	require.Error(t, err)
	require.NoError(t, os.Remove(filename))
	require.NoError(t, os.Rename(filename+".old", filename))
	require.NoError(t, os.Chmod(filepath.Join(root, "secrets"), 0755))
	_, err = client.resolve()
	require.Error(t, err)
}

func TestMissingSelectedConfigCannotFallBackToAnotherInstalledRuntime(t *testing.T) {
	client, root := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not contact a fallback runtime") })
	require.NoError(t, os.Remove(client.ConfigPath))
	// A different default runtime's credential must not rescue a missing
	// explicitly selected configuration or silently change device ownership.
	dataHome := t.TempDir()
	require.NoError(t, os.Symlink(root, filepath.Join(dataHome, "agent-mobile-runtime")))
	t.Setenv("XDG_DATA_HOME", dataHome)
	_, err := client.resolve()
	require.Error(t, err)
}

func TestInvalidSelectedConfigCannotResetToImplicitDefaults(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid configuration must fail before HTTP") })
	for _, config := range []string{`null`, `[]`, `{"dataDir":null}`, `{"dataDir":""}`, `{"controlPort":null}`, `{"controlPort":"7341"}`, `{"controlPort":7341.5}`, `{"controlPort":80}`} {
		require.NoError(t, os.WriteFile(client.ConfigPath, []byte(config), 0600))
		_, err := client.resolve()
		require.Error(t, err, config)
	}
}

func TestRedirectNeverReceivesNodeCredential(t *testing.T) {
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		t.Error("redirect must not be followed")
	}))
	t.Cleanup(target.Close)
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	})
	_, err := client.Request(context.Background(), "remote:owner", "create", raw(`{"template":"clean"}`))
	require.Error(t, err)
	require.Equal(t, int64(0), targetCalls.Load())
}

func TestScreenshotKeepsFreshFrameAndDropsPrivateResponseFields(t *testing.T) {
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jCioAAAAASUVORK5CYII="
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/admin/sessions/"+testSession+"/action", r.URL.Path)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "screenshot", body["action"])
		fmt.Fprintf(w, `{"mime":"image/png","data":%q,"width":1,"height":1,"frameId":%q,"credentialFile":"/private/file"}`, png, testFrame)
	})
	data, err := client.Request(context.Background(), "remote:owner", "screenshot", raw(`{"sessionId":"`+testSession+`"}`))
	require.NoError(t, err)
	require.Contains(t, string(data), testFrame)
	require.Contains(t, string(data), png)
	require.NotContains(t, string(data), "credentialFile")
}

func TestRecordingFailuresCannotForwardLocalPathsOrCredentials(t *testing.T) {
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[{"id":%q,"deviceId":%q,"kind":"audio","state":"failed","durationSec":3,"bytes":0,"error":"Bearer secret at /private/file","path":"/private/recording"}]`, testRecording, testSession)
	})
	data, err := client.Request(context.Background(), "remote:owner", "recording_list", raw(`{"sessionId":"`+testSession+`"}`))
	require.NoError(t, err)
	require.Contains(t, string(data), "Recording failed")
	require.NotContains(t, string(data), "Bearer")
	require.NotContains(t, string(data), "/private")
}

func TestOperationDeadlinesKeepBootLongerThanRuntimeMaximumWithoutStallingReads(t *testing.T) {
	require.Greater(t, operationTimeout("start"), 600*time.Second+60*time.Second)
	for _, operation := range []string{"state", "templates", "screenshot", "recording_list", "recording_read", "tap", "swipe", "text", "key"} {
		require.Equal(t, 15*time.Second, operationTimeout(operation))
	}
	require.Equal(t, 90*time.Second, operationTimeout("stop"))
	require.Equal(t, 90*time.Second, operationTimeout("destroy"))
	for _, operation := range []string{"takeover", "release", "recording_stop"} {
		require.Equal(t, time.Minute, operationTimeout(operation))
	}

	var requests atomic.Int64
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Request(ctx, "remote:owner", "start", raw(`{"sessionId":"`+testSession+`"}`))
	require.Error(t, err)
	require.Zero(t, requests.Load(), "an already cancelled caller must not dispatch a long boot")
}
