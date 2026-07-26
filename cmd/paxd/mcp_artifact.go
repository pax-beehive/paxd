package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/pax-beehive/paxd/internal/control"
	paxdaemon "github.com/pax-beehive/paxd/internal/daemon"
	"github.com/pax-beehive/paxd/internal/localapi"
)

const envPaxdControlSocket = "PAXD_CONTROL_SOCKET"

var publishArtifactThroughDaemon = func(
	ctx context.Context,
	req control.PublishArtifactRequest,
) (control.ArtifactPublication, error) {
	socketPath := strings.TrimSpace(os.Getenv(envPaxdControlSocket))
	if socketPath == "" {
		socketPath = paxdaemon.DefaultControlSocketPath()
	}
	return localapi.NewUnixClient(socketPath).PublishArtifact(ctx, req)
}

func callConversationMCPPublishArtifact(
	ctx context.Context,
	args map[string]any,
) mcpToolCallResult {
	agentID, _, sessionID, err := conversationMCPIdentity()
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	path := strings.TrimSpace(mcpStringArg(args, "path"))
	if path == "" {
		return mcpTextResult("path is required", true)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return mcpTextResult("resolve artifact path: "+err.Error(), true)
	}
	publication, err := publishArtifactThroughDaemon(ctx, control.PublishArtifactRequest{
		AgentID:    agentID,
		SessionID:  sessionID,
		SourcePath: path,
		Title:      mcpStringArg(args, "title"),
	})
	if err != nil {
		return mcpTextResult(err.Error(), true)
	}
	result := map[string]any{
		"accepted": true,
		"pax_artifact_publication": map[string]string{
			"publication_id": publication.PublicationID,
		},
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return mcpTextResult("failed to encode artifact publication result", true)
	}
	return mcpTextResult(string(raw), false)
}
