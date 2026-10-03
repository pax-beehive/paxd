package artifactpublisher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pax-beehive/paxd/internal/daemonstore"
	"github.com/pax-beehive/paxd/internal/noderouting"
	"github.com/pax-beehive/paxd/internal/safehttp"
)

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type HTTPManager struct {
	client HTTPDoer
}

func NewHTTPManager(client HTTPDoer) *HTTPManager {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: &noderouting.Transport{}}
	}
	return &HTTPManager{client: client}
}

func (m *HTTPManager) Register(
	ctx context.Context,
	target ManagerTarget,
	job daemonstore.ArtifactPublishJob,
) error {
	return m.do(
		ctx,
		target,
		http.MethodPut,
		"/api/v1/node/artifact-publications/"+url.PathEscape(job.PublicationID),
		map[string]any{
			"source": map[string]string{
				"agent_id":   job.AgentID,
				"session_id": job.SessionID,
			},
			"filename": job.SourceFilename,
			"title":    job.DisplayTitle,
		},
		nil,
	)
}

func (m *HTTPManager) Prepare(
	ctx context.Context,
	target ManagerTarget,
	job daemonstore.ArtifactPublishJob,
) (PrepareResult, error) {
	var prepared PrepareResult
	err := m.do(
		ctx,
		target,
		http.MethodPost,
		"/api/v1/node/artifact-publications/"+url.PathEscape(job.PublicationID)+"/prepare",
		map[string]any{
			"filename":     job.SourceFilename,
			"content_type": job.ContentType,
			"size_bytes":   job.SizeBytes,
			"sha256":       job.SHA256,
		},
		&prepared,
	)
	return prepared, err
}

func (m *HTTPManager) Complete(
	ctx context.Context,
	target ManagerTarget,
	job daemonstore.ArtifactPublishJob,
) error {
	return m.do(
		ctx,
		target,
		http.MethodPost,
		"/api/v1/node/artifact-uploads/"+url.PathEscape(job.UploadID)+"/complete",
		map[string]any{},
		nil,
	)
}

func (m *HTTPManager) Fail(
	ctx context.Context,
	target ManagerTarget,
	job daemonstore.ArtifactPublishJob,
	code string,
	message string,
) error {
	return m.do(
		ctx,
		target,
		http.MethodPost,
		"/api/v1/node/artifact-publications/"+
			url.PathEscape(job.PublicationID)+"/failed",
		map[string]string{"error_code": code, "message": message},
		nil,
	)
}

func (m *HTTPManager) do(
	ctx context.Context,
	target ManagerTarget,
	method string,
	path string,
	body any,
	result any,
) error {
	if strings.TrimSpace(target.BaseURL) == "" {
		return errors.New("artifact manager base URL is empty")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal artifact manager request: %w", err)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		strings.TrimRight(target.BaseURL, "/")+path,
		bytes.NewReader(data),
	)
	if err != nil {
		return safehttp.RedactError("create artifact manager request", err)
	}
	for key, values := range target.Headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "paxd/0.1.0")
	resp, err := safehttp.DoNoRedirect(m.client, req)
	if err != nil {
		return safehttp.RedactError("request artifact manager", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			_, _ = io.Copy(io.Discard, resp.Body)
			return fmt.Errorf("artifact manager returned HTTP %d", resp.StatusCode)
		}
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return fmt.Errorf(
			"artifact manager returned HTTP %d: %s",
			resp.StatusCode,
			strings.TrimSpace(string(message)),
		)
	}
	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode artifact manager response: %w", err)
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return fmt.Errorf("decode artifact manager data: %w", err)
	}
	return nil
}
