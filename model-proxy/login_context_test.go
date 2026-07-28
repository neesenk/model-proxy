package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

type loginRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f loginRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type closeNotifyBody struct {
	io.Reader
	closed chan<- struct{}
}

func (b *closeNotifyBody) Close() error {
	b.closed <- struct{}{}
	return nil
}

func pendingLoginResponse(req *http.Request, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     make(http.Header),
		Body:       body,
		Request:    req,
	}
}

func waitLoginResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("login poll did not stop after context cancellation")
		return nil
	}
}

func TestAqpPollAtContext_CancelsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	client := &AqpClient{HTTP: &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}}

	result := make(chan error, 1)
	go func() {
		_, err := client.pollAtContext(ctx, "https://aqp.invalid/auth/info", time.Minute)
		result <- err
	}()
	<-entered
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("pollAtContext error = %v, want context.Canceled", err)
	}
}

func TestAqpPollAtContext_CancelsRetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	bodyClosed := make(chan struct{}, 1)
	var calls atomic.Int32
	client := &AqpClient{HTTP: &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return pendingLoginResponse(req, &closeNotifyBody{
			Reader: strings.NewReader(`{"retcode":1,"message":"pending"}`),
			closed: bodyClosed,
		}), nil
	})}}

	result := make(chan error, 1)
	go func() {
		_, err := client.pollAtContext(ctx, "https://aqp.invalid/auth/info", time.Minute)
		result <- err
	}()
	<-bodyClosed
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("pollAtContext error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("auth/info calls = %d, want 1", got)
	}
}

func TestPollForTokenContext_CancelsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	opts := &codexLoginServerOptions{
		deviceTokURL: "https://auth.invalid/device-token",
		httpClient: &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			entered <- struct{}{}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
	}

	result := make(chan error, 1)
	go func() {
		_, err := pollForTokenContext(ctx, opts, "device-auth-id", "user-code", 60)
		result <- err
	}()
	<-entered
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("pollForTokenContext error = %v, want context.Canceled", err)
	}
}

func TestPollForTokenContext_CancelsRetryWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	bodyClosed := make(chan struct{}, 1)
	var calls atomic.Int32
	opts := &codexLoginServerOptions{
		deviceTokURL: "https://auth.invalid/device-token",
		httpClient: &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return pendingLoginResponse(req, &closeNotifyBody{
				Reader: strings.NewReader(`{"error":{"code":"deviceauth_authorization_pending"}}`),
				closed: bodyClosed,
			}), nil
		})},
	}

	result := make(chan error, 1)
	go func() {
		_, err := pollForTokenContext(ctx, opts, "device-auth-id", "user-code", 60)
		result <- err
	}()
	<-bodyClosed
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("pollForTokenContext error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("device-token calls = %d, want 1", got)
	}
}

func TestAqpFetchAPIKeyAtContext_CancelsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	storePath := t.TempDir() + "/aqp_oauth_auth.json"
	if err := provider.SaveAqpAccount(storePath, &provider.AqpAccountData{
		SSOSessionCookie: "SSO_C=test-session",
	}); err != nil {
		t.Fatal(err)
	}
	client := newAqpClient(storePath)
	client.HTTP = &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}

	result := make(chan error, 1)
	go func() {
		_, err := client.fetchAPIKeyAtContext(ctx, "https://aqp.invalid/api-key")
		result <- err
	}()
	<-entered
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("fetchAPIKeyAtContext error = %v, want context.Canceled", err)
	}
}

func TestExchangeCodeForTokensContext_CancelsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	opts := &codexLoginServerOptions{
		tokenURL: "https://auth.invalid/oauth/token",
		httpClient: &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			entered <- struct{}{}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
	}

	result := make(chan error, 1)
	go func() {
		_, err := exchangeCodeForTokensContext(ctx, opts, provider.CodexOAuthClientID, "authorization-code", "verifier")
		result <- err
	}()
	<-entered
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("exchangeCodeForTokensContext error = %v, want context.Canceled", err)
	}
}

func TestAqpBootstrapAtContext_CancelsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	client := newAqpClient(t.TempDir() + "/aqp_oauth_auth.json")
	client.HTTP = &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}

	result := make(chan error, 1)
	go func() {
		_, err := client.bootstrapAtContext(ctx, "https://aqp.invalid/auth/login")
		result <- err
	}()
	<-entered
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("bootstrapAtContext error = %v, want context.Canceled", err)
	}
}

func TestRequestUserCodeContext_CancelsInFlightRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{}, 1)
	opts := &codexLoginServerOptions{
		usercodeURL: "https://auth.invalid/device-code",
		httpClient: &http.Client{Transport: loginRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			entered <- struct{}{}
			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
	}

	result := make(chan error, 1)
	go func() {
		_, err := requestUserCodeContext(ctx, opts, provider.CodexOAuthClientID)
		result <- err
	}()
	<-entered
	cancel()

	if err := waitLoginResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("requestUserCodeContext error = %v, want context.Canceled", err)
	}
}
