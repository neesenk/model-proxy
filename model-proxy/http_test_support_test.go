package main

import (
	"encoding/json"
	"net/http"
)

// writeJSON is a test-server helper retained in the composition-root test
// package. Production JSON presentation belongs to internal/web.
func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("content-type", "application/json")
	response.WriteHeader(status)
	data, err := json.Marshal(value)
	if err != nil {
		panic("marshal test JSON: " + err.Error())
	}
	_, _ = response.Write(data)
}
