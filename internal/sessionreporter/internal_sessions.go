package sessionreporter

import "strings"

// Manager reserves this namespace for internal Secretary executions.
func isSecretaryWorkSession(managerSessionID string) bool {
	return strings.HasPrefix(strings.TrimSpace(managerSessionID), "sess_secretary_")
}
