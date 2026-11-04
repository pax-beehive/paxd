package cloud

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"10", 10 * time.Second},
		{" 60 ", time.Minute},
		{"0", time.Second},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{now.Add(-time.Hour).Format(http.TimeFormat), time.Second},
		{"", 10 * time.Second},
		{"invalid", 10 * time.Second},
		{"-1", 10 * time.Second},
		{"1.5", 10 * time.Second},
		{"9223372036854775807", time.Duration(1<<63 - 1)},
		{"999999999999999999999999999999", time.Duration(1<<63 - 1)},
	} {
		t.Run(tt.value, func(t *testing.T) {
			require.Equal(t, tt.want, parseRetryAfter(tt.value, now))
		})
	}
}

func TestRegistrationRateLimitPreservesDelayWithoutResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "10")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("<html>1015 sensitive-response</html>"))
	}))
	defer server.Close()
	client := NewClient(server.URL, "")
	_, startErr := client.StartNodeRegistration(&StartNodeRegistrationRequest{})
	_, pollErr := client.PollNodeRegistration(&PollNodeRegistrationRequest{})
	for _, err := range []error{startErr, pollErr} {
		var limited *RateLimitError
		require.ErrorAs(t, err, &limited)
		require.Equal(t, 10*time.Second, limited.RetryAfter)
		require.Contains(t, err.Error(), "retry after 10s")
		require.NotContains(t, err.Error(), "sensitive-response")
	}
}
