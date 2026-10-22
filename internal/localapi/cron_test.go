package localapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/pax-beehive/paxd/internal/cronjob"
	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/stretchr/testify/require"
)

func TestCronAPIManagesTasksAndReportsValidationAndConflicts(t *testing.T) {
	db, err := daemonstore.OpenSQLite(filepath.Join(t.TempDir(), "paxd.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	svc, err := cronjob.New(db.DB(), cronjob.Options{})
	require.NoError(t, err)
	handler := NewHandlerWithCron(docsRouteService{}, nil, svc)
	request := func(method, path string, body any, status int, result any) {
		t.Helper()
		data, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, status, rec.Code, rec.Body.String())
		if result != nil {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), result))
		}
	}
	create := cronjob.CreateRequest{ConnectionID: "conn_test", Name: "Daily", Agent: "codex", Prompt: "Review.", WorkingDir: t.TempDir(), Schedule: "0 9 * * *", Timezone: "UTC"}
	var job cronjob.Job
	request("POST", "/v1/cron-jobs", &create, http.StatusOK, &job)
	require.NotEmpty(t, job.ID)
	path := "/v1/cron-jobs/" + job.ID
	request("PATCH", path, map[string]any{"expected_revision": job.Revision, "enabled": false}, http.StatusOK, &job)
	require.False(t, job.Enabled)
	require.Equal(t, "Review.", job.Prompt)
	request("PATCH", path, map[string]any{"expected_revision": 1, "name": "Stale"}, http.StatusConflict, nil)
	request("PATCH", path, map[string]any{"expected_revision": job.Revision, "schedule": "bad"}, http.StatusBadRequest, nil)
	var jobs []cronjob.Job
	request("GET", "/v1/cron-jobs?agent=codex", nil, http.StatusOK, &jobs)
	require.Len(t, jobs, 1)
	var run cronjob.Run
	request("POST", path+"/run", nil, http.StatusOK, &run)
	require.Equal(t, "queued", run.Status)
	request("POST", path+"/run", nil, http.StatusConflict, nil)
	var runs []cronjob.Run
	request("GET", "/v1/cron-runs?job_id="+job.ID, nil, http.StatusOK, &runs)
	require.Len(t, runs, 1)
	request("DELETE", path, nil, http.StatusOK, nil)
	request("GET", path, nil, http.StatusNotFound, nil)
	create.SessionMode = "invalid"
	request("POST", "/v1/cron-jobs", &create, http.StatusBadRequest, nil)
}
