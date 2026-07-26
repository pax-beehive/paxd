package control

import "context"

type PublishArtifactRequest struct {
	AgentID    string `json:"agent_id"`
	SessionID  string `json:"session_id"`
	SourcePath string `json:"path"`
	Title      string `json:"title,omitempty"`
}

type ArtifactPublication struct {
	PublicationID string `json:"publication_id"`
	Status        string `json:"status"`
	Filename      string `json:"filename,omitempty"`
}

type ArtifactPublicationService interface {
	PublishArtifact(
		context.Context,
		PublishArtifactRequest,
	) (ArtifactPublication, error)
}
