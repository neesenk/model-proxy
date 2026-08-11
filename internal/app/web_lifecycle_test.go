package app

import (
	"encoding/json"
	"errors"
	"io"
	cliframework "model-proxy/internal/cli/framework"
	clilogin "model-proxy/internal/cli/login"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWebCloseCancelsAqpLoginBeforeCredentialCommit(t *testing.T) {
	setPoolHome(t, t.TempDir())
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://unused.invalid"}
	p.mu.Unlock()
	w.newAqpClientFn = func(storePath string) *clilogin.AqpClient {
		client := clilogin.NewAqpClientWithBase(storePath, "https://aqp.invalid")
		client.HTTP.Transport = webLifecycleRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case clilogin.AqpAuthLoginPath:
				return webLifecycleResponse(req, http.StatusUnauthorized, `{"result":"https://login.invalid"}`), nil
			case clilogin.AqpAuthInfoPath:
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
	w.Serve(rec, httptest.NewRequest(http.MethodPost, "/api/login/aqp/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sessionID := decodeLoginSessionID(t, rec)
	waitForSignal(t, pollStarted, "AQP poll did not start")

	closeDone := make(chan struct{})
	go func() {
		w.Close()
		close(closeDone)
	}()
	waitForSignal(t, pollCancelled, "AQP poll request was not cancelled")
	waitForSignal(t, closeDone, "Web close did not join the cancelled AQP poll")

	assertLoginCancelledWithoutCredential(t, w, sessionID, cliframework.AuthFilePath("aqp", "oauth_auth"))
}

func TestWebCloseCancelsCodexLoginBeforeCredentialCommit(t *testing.T) {
	setPoolHome(t, t.TempDir())
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://unused.invalid"}
	p.mu.Unlock()
	w.newCodexOptions = func() *clilogin.CodexLoginServerOptions {
		return &clilogin.CodexLoginServerOptions{
			UsercodeURL:  "https://auth.invalid/usercode",
			DeviceTokURL: "https://auth.invalid/devtok",
			TokenURL:     "https://auth.invalid/token",
			HTTPClient: &http.Client{Transport: webLifecycleRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
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
	w.Serve(rec, httptest.NewRequest(http.MethodPost, "/api/login/codex/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sessionID := decodeLoginSessionID(t, rec)
	waitForSignal(t, pollStarted, "Codex poll did not start")

	closeDone := make(chan struct{})
	go func() {
		w.Close()
		close(closeDone)
	}()
	waitForSignal(t, pollCancelled, "Codex poll request was not cancelled")
	waitForSignal(t, closeDone, "Web close did not join the cancelled Codex poll")

	assertLoginCancelledWithoutCredential(t, w, sessionID, cliframework.AuthFilePath("codex", "oauth_auth"))
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

func assertLoginCancelledWithoutCredential(t *testing.T, w *WebServer, sessionID, path string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	w.Serve(
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/login/"+sessionID+"/poll", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf(
			"cancelled login poll status = %d, body = %s",
			recorder.Code,
			recorder.Body.String(),
		)
	}
	var session struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode cancelled login poll: %v", err)
	}
	if session.State != "error" {
		t.Fatalf("cancelled login state = %q, want error", session.State)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential was persisted before commit boundary: stat error = %v", err)
	}
}
