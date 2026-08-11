package protocol

import (
	"net/http"

	sonic "github.com/bytedance/sonic"
)

// writeUnsupportedConversionError uses the client's native HTTP error envelope.
// Request-shape failures are JSON even when stream:true because no stream began.
func WriteUnsupportedConversionError(w http.ResponseWriter, proto Protocol, err *UnsupportedError) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	var body any
	if proto == Anthropic {
		body = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": err.Error(),
			},
		}
	} else {
		body = map[string]any{
			"error": map[string]any{
				"type":    "invalid_request_error",
				"code":    "unsupported_protocol_conversion",
				"message": err.Error(),
			},
		}
	}
	encoded, _ := sonic.Marshal(body)
	_, _ = w.Write(encoded)
}
