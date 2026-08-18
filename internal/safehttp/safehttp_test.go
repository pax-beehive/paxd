package safehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoNoRedirectGivenHTTPRedirectThenReturnsItWithoutContactingTarget(t *testing.T) {
	t.Run("Given a bearer URL redirects When requested Then the redirect is not followed", func(t *testing.T) {
		targetCalls := 0
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			targetCalls++
			w.WriteHeader(http.StatusOK)
		}))
		defer target.Close()

		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}))
		defer redirect.Close()

		req, err := http.NewRequest(http.MethodGet, redirect.URL+"/object?X-Amz-Signature=top-secret", nil)
		require.NoError(t, err)

		resp, err := DoNoRedirect(redirect.Client(), req)

		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Zero(t, targetCalls)
	})
}

func TestDoNoRedirectDoesNotMutateTheCallersClient(t *testing.T) {
	client := &http.Client{}
	req, err := http.NewRequest(http.MethodGet, "https://objects.example.test/object", nil)
	require.NoError(t, err)

	_, _ = DoNoRedirect(client, req)

	assert.Nil(t, client.CheckRedirect)
}

func TestRedactErrorHidesURLAndPreservesCancellationClassification(t *testing.T) {
	secretURL := "https://bucket.example/object?X-Amz-Credential=credential&X-Amz-Signature=top-secret"
	cause := &url.Error{
		Op:  http.MethodGet,
		URL: secretURL,
		Err: context.DeadlineExceeded,
	}

	err := RedactError("download signed object", cause)

	require.Error(t, err)
	assert.Equal(t, "download signed object failed", err.Error())
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NotContains(t, err.Error(), secretURL)
	assert.NotContains(t, strings.ToLower(err.Error()), "x-amz-signature")
	assert.NotContains(t, err.Error(), "top-secret")
	assert.Nil(t, errors.Unwrap(err), "the sensitive transport cause must not be externally exposed")
}

func TestRedactErrorGivenNilCauseThenReturnsNil(t *testing.T) {
	assert.NoError(t, RedactError("request", nil))
}
