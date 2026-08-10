package main

import (
	"encoding/json"
	"net/http"
)

// writeJSON is the root test-server JSON helper.
func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("content-type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
