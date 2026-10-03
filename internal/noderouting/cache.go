// Package noderouting stores disposable routing hints, never credentials.
package noderouting

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const HeaderUserID = "X-Pax-User-ID"

var validUserID = regexp.MustCompile(`^usr_[A-Za-z0-9_-]{1,124}$`)

type Cache struct{ Dir string }

func DefaultCache() *Cache {
	home, err := os.UserHomeDir()
	if err != nil {
		return &Cache{}
	}
	return &Cache{Dir: filepath.Join(home, ".paxd", "cache", "node-routing")}
}

func (c *Cache) filename(rawURL, key string) string {
	if c == nil || c.Dir == "" || key == "" {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return ""
	}
	// Scope the hint to both service and credential without storing the key.
	sum := sha256.Sum256([]byte("pax-node-routing-v1\x00" + u.Scheme + "://" + u.Host + "\x00" + key))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:])+".txt")
}

func (c *Cache) Load(rawURL, key string) string {
	path := c.filename(rawURL, key)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 130 {
		return ""
	}
	data, err := os.ReadFile(path)
	value := strings.TrimSpace(string(data))
	if err != nil || !validUserID.MatchString(value) {
		return ""
	}
	return value
}

// Save is best-effort: losing the cache must never prevent authentication.
func (c *Cache) Save(rawURL, key, userID string) {
	path := c.filename(rawURL, key)
	if path == "" || !validUserID.MatchString(userID) || c.Load(rawURL, key) == userID {
		return
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return
	}
	info, err := os.Lstat(c.Dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	file, err := os.CreateTemp(c.Dir, ".hint-*")
	if err != nil {
		return
	}
	temp := file.Name()
	defer os.Remove(temp)
	_, writeErr := io.WriteString(file, userID+"\n")
	closeErr := file.Close()
	if writeErr == nil && closeErr == nil {
		_ = os.Rename(temp, path)
	}
}

func machineRequest(rawURL string, header http.Header) bool {
	u, err := url.Parse(rawURL)
	return err == nil && header.Get("X-Pax-Key") != "" &&
		(strings.HasPrefix(u.Path, "/api/v1/node/") || u.Path == "/api/v1/agent/tunnel")
}

func (c *Cache) Headers(rawURL string, header http.Header) http.Header {
	result := header.Clone()
	if result == nil {
		result = make(http.Header)
	}
	result.Del(HeaderUserID)
	if machineRequest(rawURL, result) {
		if hint := c.Load(rawURL, result.Get("X-Pax-Key")); hint != "" {
			result.Set(HeaderUserID, hint)
		}
	}
	return result
}

func (c *Cache) Observe(rawURL string, header http.Header, response *http.Response) {
	if response == nil || !machineRequest(rawURL, header) ||
		(response.StatusCode != http.StatusSwitchingProtocols &&
			(response.StatusCode < 200 || response.StatusCode >= 300)) {
		return
	}
	c.Save(rawURL, header.Get("X-Pax-Key"), response.Header.Get(HeaderUserID))
}

// Transport never retries or changes an upstream URL. Recovery is server-side.
type Transport struct {
	Base  http.RoundTripper
	Cache *Cache
}

func (t *Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	cache := t.Cache
	if cache == nil {
		cache = DefaultCache()
	}
	clone := request.Clone(request.Context())
	clone.Header = cache.Headers(request.URL.String(), request.Header)
	response, err := base.RoundTrip(clone)
	if err == nil {
		cache.Observe(request.URL.String(), clone.Header, response)
	}
	return response, err
}
