package remotelogin

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pax-beehive/paxd/internal/cloud"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginPrintsPairingInfoOpensBrowserAndReturnsNodeKey(t *testing.T) {
	client := &fakeCloudClient{
		start: &cloud.StartNodeRegistrationResponse{
			RegistrationID:          "reg_123",
			PairCode:                "ABC123",
			PollToken:               "poll-secret",
			VerificationURIComplete: "https://ws.example.test/connect.html?code=ABC123",
			Interval:                1,
		},
		polls: []*cloud.PollNodeRegistrationResponse{
			{Status: "pending"},
			{Status: "approved", NodeID: "node_123", APIKey: "node-key"},
		},
	}
	var stdout bytes.Buffer
	var opened []string
	var sleeps []time.Duration

	result, err := Login(context.Background(), LoginSpec{
		RemoteID:    "prod",
		CloudAPIURL: "https://api.example.test",
		Node: NodeRegistrationInfo{
			Hostname:    "host-a",
			OS:          "darwin",
			Arch:        "arm64",
			PaxdVersion: "test",
		},
	}, Options{
		Client: client,
		Stdout: &stdout,
		OpenBrowser: func(ctx context.Context, url string) error {
			opened = append(opened, url)
			return nil
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			return nil
		},
	})
	require.NoError(t, err)

	assert.Equal(t, LoginResult{
		RemoteID:    "prod",
		CloudAPIURL: "https://api.example.test",
		NodeID:      "node_123",
		NodeAPIKey:  "node-key",
	}, result)
	assert.Equal(t, []string{"https://ws.example.test/connect.html?code=ABC123"}, opened)
	assert.Equal(t, []time.Duration{time.Second}, sleeps)
	assert.Contains(t, stdout.String(), "Opening browser for Pax login")
	assert.Contains(t, stdout.String(), "Code: ABC123")
	assert.Contains(t, stdout.String(), "Waiting for approval")
	assert.NotContains(t, stdout.String(), "poll-secret")
	assert.Equal(t, "poll-secret", client.pollRequests[0].PollToken)
	assert.Equal(t, "reg_123", client.pollRequests[0].RegistrationID)
}

func TestLoginDoesNotFailWhenBrowserOpenFails(t *testing.T) {
	client := &fakeCloudClient{
		start: &cloud.StartNodeRegistrationResponse{
			RegistrationID:          "reg_123",
			PairCode:                "ABC123",
			PollToken:               "poll-secret",
			VerificationURIComplete: "https://ws.example.test/connect.html?code=ABC123",
		},
		polls: []*cloud.PollNodeRegistrationResponse{
			{Status: "approved", NodeID: "node_123", APIKey: "node-key"},
		},
	}

	result, err := Login(context.Background(), LoginSpec{
		RemoteID:    "prod",
		CloudAPIURL: "https://api.example.test",
	}, Options{
		Client: client,
		OpenBrowser: func(ctx context.Context, url string) error {
			return errors.New("browser unavailable")
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "node_123", result.NodeID)
}

func TestLoginReturnsRecoverableErrorsForDeniedExpiredAndCanceled(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   error
	}{
		{name: "denied", status: "denied", want: ErrDenied},
		{name: "expired", status: "expired", want: ErrExpired},
		{name: "canceled", status: "canceled", want: ErrCanceled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeCloudClient{
				start: &cloud.StartNodeRegistrationResponse{
					RegistrationID: "reg_123",
					PairCode:       "ABC123",
					PollToken:      "poll-secret",
				},
				polls: []*cloud.PollNodeRegistrationResponse{{Status: test.status}},
			}

			_, err := Login(context.Background(), LoginSpec{
				RemoteID:    "prod",
				CloudAPIURL: "https://api.example.test",
			}, Options{Client: client})

			require.ErrorIs(t, err, test.want)
		})
	}
}

func TestLoginRequiresApprovedNodeCredentials(t *testing.T) {
	client := &fakeCloudClient{
		start: &cloud.StartNodeRegistrationResponse{
			RegistrationID: "reg_123",
			PairCode:       "ABC123",
			PollToken:      "poll-secret",
		},
		polls: []*cloud.PollNodeRegistrationResponse{{Status: "approved"}},
	}

	_, err := Login(context.Background(), LoginSpec{
		RemoteID:    "prod",
		CloudAPIURL: "https://api.example.test",
	}, Options{Client: client})

	require.Error(t, err)
}

func TestLoginReturnsStartAndPollErrors(t *testing.T) {
	startErr := errors.New("start failed")
	_, err := Login(context.Background(), LoginSpec{RemoteID: "prod"}, Options{
		Client: &fakeCloudClient{startErr: startErr},
	})
	require.ErrorIs(t, err, startErr)

	pollErr := errors.New("poll failed")
	_, err = Login(context.Background(), LoginSpec{RemoteID: "prod"}, Options{
		Client: &fakeCloudClient{
			start: &cloud.StartNodeRegistrationResponse{
				RegistrationID: "reg_123",
				PairCode:       "ABC123",
				PollToken:      "poll-secret",
			},
			pollErr: pollErr,
		},
	})
	require.ErrorIs(t, err, pollErr)
}

func TestLoginRejectsMissingClientAndEmptyResponses(t *testing.T) {
	_, err := Login(context.Background(), LoginSpec{RemoteID: "prod"}, Options{})
	require.Error(t, err)

	_, err = Login(context.Background(), LoginSpec{RemoteID: "prod"}, Options{
		Client: &fakeCloudClient{},
	})
	require.Error(t, err)

	_, err = Login(context.Background(), LoginSpec{RemoteID: "prod"}, Options{
		Client: &fakeCloudClient{
			start: &cloud.StartNodeRegistrationResponse{
				RegistrationID: "reg_123",
				PairCode:       "ABC123",
				PollToken:      "poll-secret",
			},
			polls: []*cloud.PollNodeRegistrationResponse{nil},
		},
	})
	require.Error(t, err)
}

func TestLoginUsesVerificationURIWhenCompleteURLIsMissing(t *testing.T) {
	client := &fakeCloudClient{
		start: &cloud.StartNodeRegistrationResponse{
			RegistrationID:  "reg_123",
			PairCode:        "ABC123",
			PollToken:       "poll-secret",
			VerificationURI: "https://ws.example.test/connect.html",
		},
		polls: []*cloud.PollNodeRegistrationResponse{
			{Status: "approved", NodeID: "node_123", APIKey: "node-key"},
		},
	}
	var stdout bytes.Buffer
	var opened []string

	_, err := Login(context.Background(), LoginSpec{RemoteID: "prod"}, Options{
		Client: client,
		Stdout: &stdout,
		OpenBrowser: func(ctx context.Context, url string) error {
			opened = append(opened, url)
			return nil
		},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"https://ws.example.test/connect.html"}, opened)
	assert.Contains(t, stdout.String(), "https://ws.example.test/connect.html")
}

func TestSleepAndOpenBrowserHandleCancellationAndEmptyURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := Sleep(ctx, time.Hour)
	require.ErrorIs(t, err, context.Canceled)

	err = OpenBrowser(context.Background(), "")
	require.NoError(t, err)
}

type fakeCloudClient struct {
	start        *cloud.StartNodeRegistrationResponse
	startErr     error
	polls        []*cloud.PollNodeRegistrationResponse
	pollErr      error
	pollRequests []cloud.PollNodeRegistrationRequest
}

func (c *fakeCloudClient) StartNodeRegistration(req *cloud.StartNodeRegistrationRequest) (*cloud.StartNodeRegistrationResponse, error) {
	if c.startErr != nil {
		return nil, c.startErr
	}
	return c.start, nil
}

func (c *fakeCloudClient) PollNodeRegistration(req *cloud.PollNodeRegistrationRequest) (*cloud.PollNodeRegistrationResponse, error) {
	c.pollRequests = append(c.pollRequests, *req)
	if c.pollErr != nil {
		return nil, c.pollErr
	}
	if len(c.polls) == 0 {
		return &cloud.PollNodeRegistrationResponse{Status: "pending"}, nil
	}
	next := c.polls[0]
	c.polls = c.polls[1:]
	return next, nil
}
