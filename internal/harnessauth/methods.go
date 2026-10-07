package harnessauth

import "github.com/pax-beehive/paxd/internal/control"

func loginMethod(req control.HarnessAuthLoginCommand) string {
	if req.Method != "" {
		return req.Method
	}
	if req.Harness == "codex" {
		return "device"
	}
	return "subscription"
}

func loginArgs(harness, method string) []string {
	if harness == "claude" {
		if method == "console" {
			return []string{"auth", "login", "--console"}
		}
		return []string{"auth", "login"}
	}
	switch method {
	case "api-key":
		return []string{"login", "--with-api-key"}
	case "access-token":
		return []string{"login", "--with-access-token"}
	default:
		return []string{"login", "--device-auth"}
	}
}

func loginStatusMatches(attempt, status control.HarnessAuthView) bool {
	if attempt.Harness != "codex" {
		return true
	}
	switch attempt.Method {
	case "api-key":
		return status.AuthMethod == "api_key"
	case "access-token":
		return status.AuthMethod == "access_token"
	default:
		return status.AuthMethod == "chatgpt"
	}
}
