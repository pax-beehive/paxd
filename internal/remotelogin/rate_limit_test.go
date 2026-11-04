package remotelogin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/stretchr/testify/require"
)

func TestLoginRecoversFromStartAndPollRateLimits(t *testing.T) {
	var starts, polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/node/registration/start":
			starts++
			if starts == 1 {
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, `{"data":{"registration_id":"reg1","poll_token":"secret","interval":2,"verification_uri":"https://example.test/verify"}}`)
		case "/api/v1/node/registration/poll":
			polls++
			var req cloud.PollNodeRegistrationRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.RegistrationID != "reg1" || req.PollToken != "secret" {
				t.Error("poll must reuse the original registration")
			}
			if polls == 1 {
				w.Header().Set("Retry-After", "12")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			fmt.Fprint(w, `{"data":{"status":"approved","node_id":"node1","api_key":"key"}}`)
		default:
			t.Error("unexpected request")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	var waits []time.Duration
	var browserOpens int
	var output bytes.Buffer
	result, err := Login(context.Background(), LoginSpec{}, Options{
		Client: cloud.NewClient(server.URL, ""),
		Stdout: &output,
		OpenBrowser: func(context.Context, string) error {
			browserOpens++
			return nil
		},
		Sleep: func(ctx context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, "node1", result.NodeID)
	require.Equal(t, 2, starts)
	require.Equal(t, 2, polls)
	require.Equal(t, 1, browserOpens)
	require.Equal(t, []time.Duration{10 * time.Second, 12 * time.Second}, waits)
	require.NotContains(t, output.String(), "secret")
}

func TestRegistrationRetryIsBoundedAndOnlyRetries429(t *testing.T) {
	for _, tt := range []struct {
		name  string
		err   error
		calls int
	}{
		{"persistent limit", &cloud.RateLimitError{RetryAfter: 10 * time.Second}, 4},
		{"long limit", &cloud.RateLimitError{RetryAfter: 2 * time.Minute}, 1},
		{"ambiguous transport failure", errors.New("connection reset"), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls, sleeps := 0, 0
			_, err := retryRegistration(context.Background(), io.Discard, func(context.Context, time.Duration) error {
				sleeps++
				return nil
			}, func() (*int, error) {
				calls++
				return nil, tt.err
			})
			require.ErrorIs(t, err, tt.err)
			require.Equal(t, tt.calls, calls)
			require.Equal(t, tt.calls-1, sleeps)
		})
	}
}

func TestRegistrationRetryCancellationStopsRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := retryRegistration(ctx, io.Discard, func(ctx context.Context, d time.Duration) error {
		cancel()
		return Sleep(ctx, d)
	}, func() (*int, error) {
		calls++
		return nil, &cloud.RateLimitError{RetryAfter: time.Minute}
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, calls)
	_, err = Login(ctx, LoginSpec{}, Options{Client: &fakeCloudClient{startErr: errors.New("must not call")}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestLoginDoesNotRetryOtherHTTPFailures(t *testing.T) {
	for _, stage := range []string{"start", "poll"} {
		for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("%s/%d", stage, status), func(t *testing.T) {
				var starts, polls int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/node/registration/start":
						starts++
						if stage == "start" {
							w.WriteHeader(status)
							return
						}
						fmt.Fprint(w, `{"data":{"registration_id":"reg1","poll_token":"secret"}}`)
					case "/api/v1/node/registration/poll":
						polls++
						w.WriteHeader(status)
					default:
						t.Error("Unexpected registration endpoint.")
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				sleeps := 0
				_, err := Login(context.Background(), LoginSpec{}, Options{
					Client: cloud.NewClient(server.URL, ""),
					Sleep:  func(context.Context, time.Duration) error { sleeps++; return nil },
				})
				require.Error(t, err)
				require.Contains(t, err.Error(), fmt.Sprintf("status %d", status))
				require.Equal(t, 1, starts)
				if stage == "poll" {
					require.Equal(t, 1, polls)
				} else {
					require.Zero(t, polls)
				}
				require.Zero(t, sleeps)
			})
		}
	}
}
