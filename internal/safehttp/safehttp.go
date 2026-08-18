// Package safehttp contains the HTTP boundary used for bearer URLs. Redirects
// are deliberately returned to the caller instead of followed: a presigned
// URL is a credential, and following it can disclose that credential through
// the Referer header or hide an interactive-login response behind a final 200.
package safehttp

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Doer is the subset of http.Client required by DoNoRedirect.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// DoNoRedirect executes req without following an HTTP redirect. A caller-owned
// *http.Client is shallow-cloned so its redirect policy is not mutated. Custom
// Doer implementations remain useful as deterministic test doubles.
func DoNoRedirect(client Doer, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if httpClient, ok := client.(*http.Client); ok {
		cloned := *httpClient
		cloned.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		return cloned.Do(req)
	}
	return client.Do(req)
}

// RedactError replaces an HTTP/request error with stable text. net/http often
// wraps transport failures in *url.Error, whose Error method includes the full
// request URL and therefore any presigned query credential. The cause remains
// private, while errors.Is still supports cancellation and sentinel checks.
func RedactError(operation string, cause error) error {
	if cause == nil {
		return nil
	}
	operation = strings.TrimSpace(operation)
	if operation == "" {
		operation = "HTTP request"
	}
	return redactedError{operation: operation, cause: classificationCause(cause)}
}

type redactedError struct {
	operation string
	cause     error
}

func (e redactedError) Error() string {
	return e.operation + " failed"
}

func (e redactedError) Is(target error) bool {
	return errors.Is(e.cause, target)
}

func classificationCause(cause error) error {
	for {
		var requestErr *url.Error
		if !errors.As(cause, &requestErr) || requestErr.Err == nil || requestErr.Err == cause {
			return cause
		}
		cause = requestErr.Err
	}
}
