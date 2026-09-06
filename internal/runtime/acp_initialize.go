package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

const internalACPInitializeID = "paxd.initialize"

type acpRPCMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *acpRPCError    `json:"error,omitempty"`
}

type acpRPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type acpClientInitProfile struct {
	Params      json.RawMessage
	ProfileHash string
}

type acpWorkerInitResult struct {
	Result        json.RawMessage
	ResultHash    string
	InitializedAt time.Time
}

type acpInitCapture struct {
	err error
}

func buildACPClientInitProfile(paxdVersion string) (acpClientInitProfile, error) {
	params, err := canonicalJSON(map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"session": map[string]any{
				"configOptions": map[string]any{
					"boolean": map[string]any{},
				},
			},
		},
		"clientInfo": map[string]any{
			"name":    "paxd",
			"version": StaticPaxdVersionProvider(paxdVersion).PaxdVersion(),
		},
	})
	if err != nil {
		return acpClientInitProfile{}, err
	}
	return acpClientInitProfile{
		Params:      params,
		ProfileHash: hashBytes(params),
	}, nil
}

func buildInternalACPInitializeRequest(profile acpClientInitProfile) ([]byte, error) {
	return json.Marshal(acpRPCMessage{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%q", internalACPInitializeID)),
		Method:  "initialize",
		Params:  profile.Params,
	})
}

func buildInternalACPInitializedNotification() ([]byte, error) {
	return json.Marshal(acpRPCMessage{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
		Params:  json.RawMessage(`{}`),
	})
}

func parseACPRPCMessage(payload []byte) (acpRPCMessage, bool) {
	var msg acpRPCMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return acpRPCMessage{}, false
	}
	return msg, true
}

func isInternalACPInitializeResponse(msg acpRPCMessage) bool {
	return msg.Method == "" && sameJSONRPCID(msg.ID, json.RawMessage(fmt.Sprintf("%q", internalACPInitializeID)))
}

func acpInitializeResponsePayload(request []byte, result json.RawMessage) ([]byte, bool, error) {
	msg, ok := parseACPRPCMessage(request)
	if !ok || msg.Method != "initialize" {
		return nil, false, nil
	}
	if len(bytes.TrimSpace(msg.ID)) == 0 {
		return nil, true, nil
	}
	payload, err := json.Marshal(acpRPCMessage{
		JSONRPC: "2.0",
		ID:      msg.ID,
		Result:  result,
	})
	if err != nil {
		return nil, true, err
	}
	return payload, true, nil
}

func canonicalJSON(value any) (json.RawMessage, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(payload, &normalized); err != nil {
		return nil, err
	}
	out, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

func hashBytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func sameJSONRPCID(a json.RawMessage, b json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(a), bytes.TrimSpace(b))
}
