package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"model-proxy/internal/appapi"
	"model-proxy/internal/observe/logx"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".svg"):
		// Browsers refuse octet-stream images under X-Content-Type-Options:
		// nosniff, so the favicon needs its real MIME type.
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	// Marshal BEFORE committing the status line: on failure we can still send
	// a well-formed 500 instead of panicking (net/http recovers per-connection,
	// so a panic would kill this response with an empty/truncated body). A
	// marshal error means a handler passed an unsupported value type — a
	// programming bug, not a client problem.
	data, err := json.Marshal(v)
	if err != nil {
		logx.Warnf("[web] writeJSON: marshal %T: %v", v, err)
		http.Error(w, "internal encoding error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func writeJSONErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writePortErr(w http.ResponseWriter, fallback int, err error) {
	var classified *appapi.HTTPError
	if errors.As(err, &classified) && classified.Status != 0 {
		writeJSONErr(w, classified.Status, classified.Message)
		return
	}
	writeJSONErr(w, fallback, err.Error())
}

// tailFile returns the last n lines of path. It reads BACKWARDS in chunks
// from the end: daemon logs have no rotation and can reach GBs, so /api/logs
// and `serve status --logs` must cost O(n lines), not O(whole file) in memory
// and time. Trailing newlines are ignored and an empty (or newline-only) file
// yields an empty list — same semantics as the previous whole-file read.
func tailFile(path string, n int) ([]string, error) {
	if n <= 0 {
		return []string{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	const chunkSize = 64 << 10
	offset := info.Size()
	newlines := 0
	var chunks [][]byte
	// Grow the window backwards until it holds n+1 newlines (n complete lines
	// plus the partial head to drop) or the file start.
	for offset > 0 && newlines <= n {
		read := int64(chunkSize)
		if read > offset {
			read = offset
		}
		offset -= read
		chunk := make([]byte, read)
		if _, err := file.ReadAt(chunk, offset); err != nil && err != io.EOF {
			return nil, err
		}
		newlines += bytes.Count(chunk, []byte("\n"))
		chunks = append(chunks, chunk)
	}
	window := make([]byte, 0, info.Size()-offset)
	for i := len(chunks) - 1; i >= 0; i-- {
		window = append(window, chunks[i]...)
	}

	trimmed := strings.TrimRight(string(window), "\n")
	if trimmed == "" {
		// An empty (or newline-only) log file has no lines; "" would surface as
		// a phantom blank entry in the /api/logs response.
		return []string{}, nil
	}
	lines := strings.Split(trimmed, "\n")
	if offset > 0 && len(lines) > 0 {
		// The window's first line may be cut mid-line at the chunk boundary —
		// n+1 newlines guarantee n complete lines remain after dropping it.
		lines = lines[1:]
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

func parseStatsTime(v string) (int64, bool) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Unix(), true
	}
	return 0, false
}

// statsWindow resolves the from/to unix-second bounds shared by the stats,
// agents, shadow-report and analytics handlers: the window defaults to
// [now-window, now] and each side is overridden by the ?from/?to query
// params (unix seconds or RFC3339 via parseStatsTime; unparseable values
// keep the default).
func statsWindow(q url.Values, window time.Duration) (from, to int64) {
	now := time.Now()
	from, to = now.Add(-window).Unix(), now.Unix()
	if v := q.Get("from"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			from = n
		}
	}
	if v := q.Get("to"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			to = n
		}
	}
	return from, to
}

// providerModelPair keys the analytics price coverage: pricing resolves per
// (provider, model) — the alias fallback is provider-scoped — so the same
// upstream model can be priced under one provider and unpriced under another.
type providerModelPair struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func sortedProviderModels(m map[providerModelPair]bool) []providerModelPair {
	out := make([]providerModelPair, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}
