package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pax-beehive/paxd/internal/control"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandlerGivenArtifactPublicationWhenPublisherAcceptsThenReturnsAccepted(t *testing.T) {
	service := &artifactPublicationControlService{}
	handler := NewHandler(service)
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/artifact-publications",
		bytes.NewBufferString(`{
			"agent_id":"agent_1",
			"session_id":"session_1",
			"path":"/workspace/report.pdf",
			"title":"Analysis report"
		}`),
	)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	assert.Equal(t, "/workspace/report.pdf", service.request.SourcePath)
	var publication control.ArtifactPublication
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&publication))
	assert.Equal(t, "apub_local", publication.PublicationID)
	assert.Equal(t, "accepted", publication.Status)
}

type artifactPublicationControlService struct {
	request control.PublishArtifactRequest
}

func (s *artifactPublicationControlService) HandleCommand(
	context.Context,
	control.Source,
	control.Command,
) (control.CommandAck, error) {
	return control.CommandAck{}, nil
}

func (s *artifactPublicationControlService) HandleQuery(
	context.Context,
	control.Source,
	control.Query,
) (control.QueryResult, error) {
	return control.QueryResult{}, nil
}

func (s *artifactPublicationControlService) PublishArtifact(
	_ context.Context,
	req control.PublishArtifactRequest,
) (control.ArtifactPublication, error) {
	s.request = req
	return control.ArtifactPublication{
		PublicationID: "apub_local",
		Status:        "accepted",
		Filename:      "report.pdf",
	}, nil
}
