package app

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeUpstream records every request body, path and parsed model field, and
// answers with a configurable responder.
type fakeUpstream struct {
	srv        *httptest.Server
	mu         sync.Mutex
	bodies     []string
	seenModels []string
	paths      []string
}

func newFakeUpstream(t *testing.T, respond http.HandlerFunc) *fakeUpstream {
	t.Helper()
	return newHitFakeUpstream(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		respond(w, r)
	})
}

func newHitFakeUpstream(t *testing.T, respond func(hit int, w http.ResponseWriter, r *http.Request)) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.seenModels = append(f.seenModels, modelOfBody(body))
		f.paths = append(f.paths, r.URL.Path)
		hit := len(f.bodies)
		f.mu.Unlock()
		respond(hit, w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func modelOfBody(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &m)
	return m.Model
}

func (f *fakeUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeUpstream) models() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seenModels...)
}

func (f *fakeUpstream) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = nil
	f.seenModels = nil
	f.paths = nil
}

func (f *fakeUpstream) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

func staticResponder(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
		}
		w.Write([]byte(body))
	}
}

func hitScript(script func(hit int) (status int, body string, headers http.Header, delay time.Duration)) func(hit int, w http.ResponseWriter, r *http.Request) {
	return func(hit int, w http.ResponseWriter, r *http.Request) {
		status, body, headers, delay := script(hit)
		if delay > 0 {
			time.Sleep(delay)
		}
		for k, vs := range headers {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("content-type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
		}
		w.Write([]byte(body))
	}
}

type respScript struct {
	status  int
	body    string
	headers map[string]string
}

func respondScripted(scripts ...respScript) func(hit int, w http.ResponseWriter, r *http.Request) {
	return func(hit int, w http.ResponseWriter, r *http.Request) {
		i := hit - 1
		if i >= len(scripts) {
			i = len(scripts) - 1
		}
		s := scripts[i]
		if s.headers != nil {
			for k, v := range s.headers {
				w.Header().Set(k, v)
			}
		}
		if s.status != 200 {
			w.WriteHeader(s.status)
		}
		w.Write([]byte(s.body))
	}
}
