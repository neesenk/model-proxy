package web

import (
	"errors"
	"model-proxy/internal/appapi"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestContentTypeFor(t *testing.T) {
	cases := map[string]string{
		"page.html": "text/html; charset=utf-8",
		"app.js":    "text/javascript; charset=utf-8",
		"style.css": "text/css; charset=utf-8",
		"logo.png":  "application/octet-stream",
		"":          "application/octet-stream",
	}
	for name, want := range cases {
		if got := contentTypeFor(name); got != want {
			t.Errorf("contentTypeFor(%q)=%q want %q", name, got, want)
		}
	}
}

func TestParseStatsTime(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1700000000", 1700000000, true},
		{"2023-11-14T22:13:20Z", 1700000000, true},
		{"bogus", 0, false},
		{"", 0, false},
	}
	for _, current := range cases {
		got, ok := parseStatsTime(current.in)
		if ok != current.ok || (ok && got != current.want) {
			t.Errorf(
				"parseStatsTime(%q)=(%d,%v) want (%d,%v)",
				current.in,
				got,
				ok,
				current.want,
				current.ok,
			)
		}
	}
}

func TestWritePortErrClassifiesWrappedHTTPError(t *testing.T) {
	recorder := httptest.NewRecorder()
	writePortErr(
		recorder,
		http.StatusInternalServerError,
		errors.Join(errors.New("context"), appapi.NewHTTPError(http.StatusConflict, "conflict")),
	)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
	if got, want := recorder.Body.String(), `{"error":"conflict"}`; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}
