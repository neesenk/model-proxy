package daemonctl

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))
	defer srv.Close()

	body, status, err := Get(srv.URL, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTeapot || string(body) != "hello" {
		t.Errorf("Get = %d %q", status, body)
	}
}

func TestGetConnectionError(t *testing.T) {
	// Closed port: error must surface with status 0.
	_, status, err := Get("http://127.0.0.1:1", "/x")
	if err == nil {
		t.Error("want error for closed port")
	}
	if status != 0 {
		t.Errorf("status = %d, want 0 on transport error", status)
	}
}
