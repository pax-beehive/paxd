package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pax-beehive/paxd/internal/control"
)

func (h *Handler) routeArtifactPublicationCreate(w http.ResponseWriter, r *http.Request) {
	var req control.PublishArtifactRequest
	if !decodeBody(w, r, &req) {
		return
	}
	publisher, ok := h.service.(control.ArtifactPublicationService)
	if !ok {
		writeControlError(w, http.StatusInternalServerError, control.ControlError{
			Code:    control.ErrCodeInternal,
			Message: "artifact publisher is not configured",
		})
		return
	}
	publication, err := publisher.PublishArtifact(r.Context(), req)
	if err != nil {
		writeControlError(w, http.StatusBadRequest, control.ControlError{
			Code:    control.ErrCodeInvalidArgument,
			Message: err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusAccepted, publication)
}

func (c *Client) PublishArtifact(
	ctx context.Context,
	req control.PublishArtifactRequest,
) (control.ArtifactPublication, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return control.ArtifactPublication{}, err
	}
	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.baseURL+"/v1/artifact-publications",
		bytes.NewReader(raw),
	)
	if err != nil {
		return control.ArtifactPublication{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return control.ArtifactPublication{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var controlErr control.ControlError
		if err := json.NewDecoder(resp.Body).Decode(&controlErr); err == nil &&
			controlErr.Message != "" {
			return control.ArtifactPublication{}, controlErr
		}
		return control.ArtifactPublication{}, fmt.Errorf(
			"local artifact publication returned HTTP %d",
			resp.StatusCode,
		)
	}
	var publication control.ArtifactPublication
	if err := json.NewDecoder(resp.Body).Decode(&publication); err != nil {
		return control.ArtifactPublication{}, err
	}
	return publication, nil
}
