package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWebTaskOwnerCloseCancelsAndWaitsThroughAdmittedCommit(t *testing.T) {
	owner := newWebTaskOwner()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})

	if !owner.run(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		// Model a login task that crossed its credential-commit boundary just
		// before cancellation: save+reload must finish before close returns.
		<-release
	}) {
		t.Fatal("task was not admitted before close")
	}
	<-started

	closed := make(chan struct{})
	go func() {
		owner.close()
		close(closed)
	}()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel the Web task context")
	}
	select {
	case <-closed:
		t.Fatal("close returned before the admitted Web task finished")
	default:
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not return after the Web task finished")
	}
}

func TestWebTaskOwnerRejectsTasksAfterClose(t *testing.T) {
	owner := newWebTaskOwner()
	owner.close()

	ran := make(chan struct{})
	if owner.run(func(context.Context) { close(ran) }) {
		t.Fatal("task was admitted after close")
	}
	select {
	case <-ran:
		t.Fatal("rejected task still ran")
	default:
	}

	// Close is intentionally idempotent for deferred/error-path cleanup.
	owner.close()
}

func TestWebCloseCancelsAqpLoginBeforeCredentialCommit(t *testing.T) {
	setPoolHome(t, t.TempDir())
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://unused.invalid"}
	p.mu.Unlock()
	w.newAqpClientFn = func(storePath string) *AqpClient {
		client := newAqpClientWithBase(storePath, "https://aqp.invalid")
		client.HTTP.Transport = webLifecycleRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case aqpAuthLoginPath:
				return webLifecycleResponse(req, http.StatusUnauthorized, `{"result":"https://login.invalid"}`), nil
			case aqpAuthInfoPath:
				close(pollStarted)
				<-req.Context().Done()
				close(pollCancelled)
				return nil, req.Context().Err()
			default:
				t.Errorf("unexpected AQP request path %q", req.URL.Path)
				return webLifecycleResponse(req, http.StatusInternalServerError, `{}`), nil
			}
		})
		return client
	}

	rec := httptest.NewRecorder()
	w.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/api/login/aqp/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sessionID := decodeLoginSessionID(t, rec)
	waitForSignal(t, pollStarted, "AQP poll did not start")

	closeDone := make(chan struct{})
	go func() {
		w.close()
		close(closeDone)
	}()
	waitForSignal(t, pollCancelled, "AQP poll request was not cancelled")
	waitForSignal(t, closeDone, "Web close did not join the cancelled AQP poll")

	assertLoginCancelledWithoutCredential(t, w, sessionID, authFilePath("aqp", "oauth_auth"))
}

func TestWebCloseCancelsCodexLoginBeforeCredentialCommit(t *testing.T) {
	setPoolHome(t, t.TempDir())
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://unused.invalid"}
	p.mu.Unlock()
	w.newCodexOptions = func() *codexLoginServerOptions {
		return &codexLoginServerOptions{
			usercodeURL:  "https://auth.invalid/usercode",
			deviceTokURL: "https://auth.invalid/devtok",
			tokenURL:     "https://auth.invalid/token",
			httpClient: &http.Client{Transport: webLifecycleRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/usercode":
					return webLifecycleResponse(req, http.StatusOK, `{"device_auth_id":"device","user_code":"CODE","interval":"60"}`), nil
				case "/devtok":
					close(pollStarted)
					<-req.Context().Done()
					close(pollCancelled)
					return nil, req.Context().Err()
				default:
					t.Errorf("unexpected Codex request path %q", req.URL.Path)
					return webLifecycleResponse(req, http.StatusInternalServerError, `{}`), nil
				}
			})},
		}
	}

	rec := httptest.NewRecorder()
	w.handleLoginStart(rec, httptest.NewRequest(http.MethodPost, "/api/login/codex/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sessionID := decodeLoginSessionID(t, rec)
	waitForSignal(t, pollStarted, "Codex poll did not start")

	closeDone := make(chan struct{})
	go func() {
		w.close()
		close(closeDone)
	}()
	waitForSignal(t, pollCancelled, "Codex poll request was not cancelled")
	waitForSignal(t, closeDone, "Web close did not join the cancelled Codex poll")

	assertLoginCancelledWithoutCredential(t, w, sessionID, authFilePath("codex", "oauth_auth"))
}

type webLifecycleRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f webLifecycleRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func webLifecycleResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func decodeLoginSessionID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var response struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login start response: %v", err)
	}
	if response.SessionID == "" {
		t.Fatal("login start response has no session_id")
	}
	return response.SessionID
}

func waitForSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}

func assertLoginCancelledWithoutCredential(t *testing.T, w *webServer, sessionID, path string) {
	t.Helper()
	session, ok := w.sessions.get(sessionID)
	if !ok {
		t.Fatal("cancelled login session disappeared")
	}
	session.mu.Lock()
	state := session.state
	session.mu.Unlock()
	if state != "error" {
		t.Fatalf("cancelled login state = %q, want error", state)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential was persisted before commit boundary: stat error = %v", err)
	}
}
