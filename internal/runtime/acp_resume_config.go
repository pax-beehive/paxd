package runtime

import (
	"context"
	"encoding/json"
)

const acpSessionResumedMethod = "_pax/session_resumed"

// An internal resume has no Manager request ID. Publish its configuration as
// a PAX notification instead of forwarding an unsolicited JSON-RPC response.
func (r *ACPRouter) emitResumedSessionConfig(ctx context.Context, nativeSessionID string, result json.RawMessage) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(result, &fields) != nil {
		return nil
	}
	if len(fields["configOptions"]) == 0 && len(fields["models"]) == 0 {
		return nil
	}
	params, err := json.Marshal(struct {
		SessionID string          `json:"sessionId"`
		Result    json.RawMessage `json:"result"`
	}{SessionID: nativeSessionID, Result: result})
	if err != nil {
		return err
	}
	payload, err := json.Marshal(acpRPCMessage{JSONRPC: "2.0", Method: acpSessionResumedMethod, Params: params})
	if err != nil {
		return err
	}
	return r.output.EmitManagerFrame(ctx, nativeSessionID, payload)
}
