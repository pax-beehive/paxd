package daemon

import (
	"context"

	"github.com/pax-beehive/paxd/internal/control"
)

type artifactControlService struct {
	control.Service
	publications control.ArtifactPublicationService
}

func (s artifactControlService) PublishArtifact(
	ctx context.Context,
	req control.PublishArtifactRequest,
) (control.ArtifactPublication, error) {
	return s.publications.PublishArtifact(ctx, req)
}
