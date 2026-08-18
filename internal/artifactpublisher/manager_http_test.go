package artifactpublisher_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

func TestHTTPManagerGivenArtifactJobWhenPublishingThenUsesNodeScopedContracts(
	t *testing.T,
) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "node-key", r.Header.Get("X-Pax-Key"))
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/api/v1/node/artifact-publications/apub_http":
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "report.pdf", body["filename"])
			writeArtifactManagerData(t, w, map[string]any{"publication": map[string]any{}})
		case "/api/v1/node/artifact-publications/apub_http/prepare":
			writeArtifactManagerData(t, w, map[string]any{
				"status":      "upload_required",
				"artifact_id": "art_http",
				"upload": map[string]any{
					"upload_id":       "artup_http",
					"protocol":        "gcs_resumable",
					"method":          "POST",
					"url":             "https://upload.example/start",
					"chunk_alignment": 262144,
				},
			})
		case "/api/v1/node/artifact-uploads/artup_http/complete":
			writeArtifactManagerData(t, w, map[string]any{
				"status":      "available",
				"artifact_id": "art_http",
			})
		case "/api/v1/node/artifact-publications/apub_http/failed":
			writeArtifactManagerData(t, w, map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manager := artifactpublisher.NewHTTPManager(server.Client())
	target := artifactpublisher.ManagerTarget{
		BaseURL: server.URL,
		Headers: http.Header{"X-Pax-Key": []string{"node-key"}},
	}
	job := daemonstore.ArtifactPublishJob{
		PublicationID:  "apub_http",
		AgentID:        "agent_1",
		SessionID:      "sess_1",
		SourceFilename: "report.pdf",
		DisplayTitle:   "Report",
		ContentType:    "application/pdf",
		SizeBytes:      12,
		SHA256:         "sha",
		UploadID:       "artup_http",
	}

	require.NoError(t, manager.Register(context.Background(), target, job))
	prepared, err := manager.Prepare(context.Background(), target, job)
	require.NoError(t, err)
	require.NotNil(t, prepared.Upload)
	assert.Equal(t, "art_http", prepared.ArtifactID)
	assert.Equal(t, int64(262144), prepared.Upload.ChunkAlignment)
	require.NoError(t, manager.Complete(context.Background(), target, job))
	require.NoError(t, manager.Fail(context.Background(), target, job, "hash_failed", "failed"))
	assert.Equal(t, []string{
		"PUT /api/v1/node/artifact-publications/apub_http",
		"POST /api/v1/node/artifact-publications/apub_http/prepare",
		"POST /api/v1/node/artifact-uploads/artup_http/complete",
		"POST /api/v1/node/artifact-publications/apub_http/failed",
	}, paths)
}

func TestHTTPManagerGivenRedirectThenDoesNotForwardNodeCredentials(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	manager := artifactpublisher.NewHTTPManager(redirect.Client())
	secret := "cf-access-top-secret"

	err := manager.Register(context.Background(), artifactpublisher.ManagerTarget{
		BaseURL: redirect.URL,
		Headers: http.Header{"CF-Access-Client-Secret": []string{secret}},
	}, daemonstore.ArtifactPublishJob{PublicationID: "apub_redirect"})

	require.ErrorContains(t, err, "HTTP 302")
	assert.Zero(t, targetCalls.Load(), "manager credentials must not cross a redirect boundary")
	assert.NotContains(t, err.Error(), secret)
}

func TestHTTPManagerRedactsTransportURL(t *testing.T) {
	secretURL := "https://manager.example.test/api?token=top-secret"
	manager := artifactpublisher.NewHTTPManager(artifactHTTPDoerFunc(
		func(*http.Request) (*http.Response, error) {
			return nil, &url.Error{
				Op:  http.MethodPut,
				URL: secretURL,
				Err: context.DeadlineExceeded,
			}
		},
	))

	err := manager.Register(context.Background(), artifactpublisher.ManagerTarget{
		BaseURL: "https://manager.example.test",
	}, daemonstore.ArtifactPublishJob{PublicationID: "apub_transport"})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), secretURL)
	assert.NotContains(t, strings.ToLower(err.Error()), "token")
	assert.NotContains(t, err.Error(), "top-secret")
}

func writeArtifactManagerData(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
}
