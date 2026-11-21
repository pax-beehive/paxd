package runtime

import "encoding/json"

const codexTurnFailed = "codex_turn_failed"

// Codex ACP v1 reports a terminal AIR failure alongside end_turn. Only the
// typed result can indicate failure. Provider details are kept out of the error.
func codexTerminalFailure(msg acpRPCMessage) bool {
	if msg.Error != nil {
		return false
	}
	var result struct {
		StopReason string `json:"stopReason"`
		Meta       struct {
			JetBrains struct {
				Air struct {
					Version        int `json:"version"`
					SessionFailure struct {
						ID       string `json:"id"`
						Revision int    `json:"revision"`
						Severity string `json:"severity"`
					} `json:"sessionFailure"`
				} `json:"air"`
			} `json:"jetbrains"`
		} `json:"_meta"`
	}
	if json.Unmarshal(msg.Result, &result) != nil || result.StopReason != "end_turn" {
		return false
	}
	air := result.Meta.JetBrains.Air
	failure := air.SessionFailure
	return air.Version == 1 && failure.ID != "" && failure.Revision > 0 && failure.Severity == "error"
}
