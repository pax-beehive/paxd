package artifactpublisher

import (
	"context"

	"github.com/pax-beehive/paxd/internal/control"
)

func (s *Service) PublishArtifact(
	ctx context.Context,
	req control.PublishArtifactRequest,
) (control.ArtifactPublication, error) {
	return s.Accept(ctx, req)
}
