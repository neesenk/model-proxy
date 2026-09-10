package web

import (
	"errors"
	"fmt"
	"model-proxy/internal/appapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContentTypeFor(t *testing.T) {
	cases := map[string]string{
		"page.html": "text/html; charset=utf-8",
		"app.js":    "text/javascript; charset=utf-8",
		"style.css": "text/css; charset=utf-8",
		"icon.svg":  "image/svg+xml",
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

// TestTailFileLargeFileReadsBackwards: tailFile must return exactly the last n
// lines of a file far larger than the chunk window, including when the window
// boundary cuts a line mid-way.
func TestTailFileLargeFileReadsBackwards(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "big.log")
	var content strings.Builder
	for i := 1; i <= 5000; i++ { // ~200KB, several 64KB chunks
		fmt.Fprintf(&content, "line-%04d padded to a decent length with some filler text\n", i)
	}
	if err := os.WriteFile(file, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := tailFile(file, 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"line-4998 padded to a decent length with some filler text",
		"line-4999 padded to a decent length with some filler text",
		"line-5000 padded to a decent length with some filler text",
	}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("tailFile(big, 3) = %v, want %v", got, want)
	}
	// Window smaller than the file but larger than n: same result.
	if got, err := tailFile(file, 5000); err != nil || got[0] != "line-0001 padded to a decent length with some filler text" {
		t.Fatalf("tailFile(big, 5000) first = %v (err %v), want line-0001", got[0], err)
	}
}
