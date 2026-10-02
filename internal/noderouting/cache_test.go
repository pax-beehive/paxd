package noderouting

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGivenWrongUserIDCacheThenServerResponseRepairsIt(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	var hints []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "node-key", r.Header.Get("X-Pax-Key"))
		hints = append(hints, r.Header.Get(HeaderUserID))
		w.Header().Set(HeaderUserID, "usr_real")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cache.Save(server.URL, "node-key", "usr_wrong")
	client := &http.Client{Transport: &Transport{Cache: cache}}
	for range 2 {
		req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/node/identity", nil)
		require.NoError(t, err)
		req.Header.Set("X-Pax-Key", "node-key")
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		assert.Empty(t, req.Header.Get(HeaderUserID), "transport must not mutate caller headers")
	}
	assert.Equal(t, []string{"usr_wrong", "usr_real"}, hints)
	assert.Equal(t, "usr_real", cache.Load(server.URL, "node-key"))
	assert.Empty(t, cache.Load(server.URL, "other-node-key"))
	assert.Empty(t, cache.Load("https://another.example", "node-key"))
}

func TestGivenCorruptOrMissingCacheThenIdentityCanBeRelearned(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	assert.Empty(t, cache.Load("https://node.example", "key"))
	cache.Save("https://node.example", "key", "usr_original")
	files, err := os.ReadDir(cache.Dir)
	require.NoError(t, err)
	require.Len(t, files, 1)
	path := filepath.Join(cache.Dir, files[0].Name())
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.NotContains(t, path, "node.example")
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "usr_original\n", string(contents))
	require.NoError(t, os.WriteFile(path, []byte("corrupt\nuser_id"), 0o600))
	assert.Empty(t, cache.Load("https://node.example", "key"))
	cache.Save("wss://node.example", "key", "usr_restored")
	assert.Equal(t, "usr_restored", cache.Load("https://node.example", "key"))
	cache.Save("https://node.example", "key", "invalid")
	assert.Equal(t, "usr_restored", cache.Load("https://node.example", "key"))
}

func TestGivenOriginFailureThenKeepHintAndNeverReplayRequest(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		w.Header().Set(HeaderUserID, "usr_untrusted_failure")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cache.Save(server.URL, "key", "usr_keep")
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/node/status", nil)
	require.NoError(t, err)
	req.Header.Set("X-Pax-Key", "key")
	resp, err := (&http.Client{Transport: &Transport{Cache: cache}}).Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, 1, count)
	assert.Equal(t, "usr_keep", cache.Load(server.URL, "key"))
}

func TestGivenUnavailableOrUnsafeCacheThenRequestsStillAuthenticate(t *testing.T) {
	for _, rawURL := range []string{"bad%url", "/relative", "ftp://node.example", "https://user:pass@node.example"} {
		t.Run(rawURL, func(t *testing.T) {
			cache := &Cache{Dir: t.TempDir()}
			cache.Save(rawURL, "key", "usr_owner")
			assert.Empty(t, cache.Load(rawURL, "key"))
		})
	}
	blocked := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocked, []byte("existing"), 0o600))
	for _, cache := range []*Cache{nil, {}, {Dir: filepath.Join(blocked, "cache")}} {
		cache.Save("https://node.example", "key", "usr_owner")
		header := cache.Headers("https://node.example/api/v1/node/status", http.Header{"X-Pax-Key": {"key"}})
		assert.Equal(t, "key", header.Get("X-Pax-Key"))
		assert.Empty(t, header.Get(HeaderUserID))
	}
}

func TestGivenUnrelatedRequestOrUntrustedResponseThenDoNotLearnHint(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	cache.Save("https://node.example", "key", "usr_owner")
	header := http.Header{"X-Pax-Key": {"key"}, HeaderUserID: {"usr_forged"}}
	userURL := "https://node.example/api/v1/user/self/me"
	assert.Empty(t, cache.Headers(userURL, header).Get(HeaderUserID))
	assert.Empty(t, cache.Headers("https://node.example/api/v1/node/status", nil).Get(HeaderUserID))
	response := &http.Response{StatusCode: 200, Header: http.Header{HeaderUserID: {"usr_other"}}}
	cache.Observe(userURL, header, response)
	cache.Observe("https://node.example/api/v1/node/status", header, nil)
	assert.Equal(t, "usr_owner", cache.Load("https://node.example", "key"))
	path := cache.filename("https://node.example", "key")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 131)), 0o600))
	assert.Empty(t, cache.Load("https://node.example", "key"))
	cache.Save("https://node.example", "key", "usr_repaired")
	assert.Equal(t, "usr_repaired", cache.Load("https://node.example", "key"))
}
