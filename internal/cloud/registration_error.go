package cloud

import (
	"fmt"
	"net/http"
	"strings"
)

// RegistrationError contains only locally generated text and an HTTP status.
// Never include response bodies, redirect URLs, or registration credentials.
type RegistrationError struct {
	StatusCode int
	Reason     string
}

func (e *RegistrationError) Error() string {
	if e.StatusCode == 0 {
		return e.Reason
	}
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Reason)
}
func registrationStatus(resp *http.Response) error {
	reason := "registration rejected by server"
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		reason = "redirected to a login page or another endpoint; use the machine API address (wsapi), not the browser API"
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		reason = "registration token rejected; verify the account region and generate a fresh command in Console"
	case resp.StatusCode == 429:
		reason = "registration rate limited; wait before trying again"
	case resp.StatusCode >= 500:
		reason = "registration server unavailable; try again later"
	case resp.StatusCode == 200 || resp.StatusCode == 201:
		if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
			return nil
		}
		reason = "received an HTML page instead of registration data; use the machine API address (wsapi)"
	}
	return &RegistrationError{StatusCode: resp.StatusCode, Reason: reason}
}
