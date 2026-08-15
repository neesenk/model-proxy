package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"model-proxy/internal/appapi"
	"net/http"
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
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("write JSON: %v", err))
	}
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

func tailFile(path string, n int) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(b), "\n")
	if trimmed == "" {
		// An empty (or newline-only) log file has no lines; "" would surface as
		// a phantom blank entry in the /api/logs response.
		return []string{}, nil
	}
	lines := strings.Split(trimmed, "\n")
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

func mapKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
