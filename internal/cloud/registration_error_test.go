package cloud

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistrationErrorsAreActionableAndDoNotExposeCredentials(t *testing.T) {
	for _, test := range []struct {
		status                  int
		contentType, body, want string
	}{
		{302, "text/html", "secret-token", "machine API"},
		{200, "text/html", "secret-token", "HTML page"},
		{200, "application/json", "secret-token", "invalid registration response"},
		{401, "application/json", "secret-token", "account region"},
		{403, "application/json", "secret-token", "fresh command"},
		{429, "application/json", "secret-token", "rate limited"},
		{503, "application/json", "secret-token", "unavailable"},
	} {
		t.Run(fmt.Sprintf("%d-%s", test.status, test.contentType), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("X-Registration-Token") != "secret-token" {
					t.Error("missing token")
				}
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("Location", "/login?token=secret-token")
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			_, err := NewClient(server.URL, "").RegisterNode(&RegisterNodeRequest{}, "secret-token")
			var registrationError *RegistrationError
			if !errors.As(err, &registrationError) {
				t.Fatalf("expected typed error, got %v", err)
			}
			if registrationError.StatusCode != test.status || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Fatal("credential leaked")
			}
			if calls != 1 {
				t.Fatalf("registration request replayed: %d", calls)
			}
		})
	}
}
