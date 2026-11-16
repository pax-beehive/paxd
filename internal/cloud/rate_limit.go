package cloud

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitError preserves the server's retry delay without exposing its body.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("status 429: rate limited; retry after %s", e.RetryAfter)
}

func registrationRateLimit(resp *http.Response) error {
	if resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	return &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	const fallback = 10 * time.Second
	value = strings.TrimSpace(value)
	if value != "" && strings.Trim(value, "0123456789") == "" {
		const maxDuration = time.Duration(1<<63 - 1)
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(maxDuration/time.Second) {
			return maxDuration
		}
		return max(time.Second, time.Duration(seconds)*time.Second)
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(time.Second, date.Sub(now))
	}
	return fallback
}
