package artifactpublisher_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func writeArtifactManagerData(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": data}))
}
