package protocol

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteUnsupportedConversionError_ProtocolEnvelopes: the unsupported-
// conversion 400 must use the CLIENT protocol's native error envelope in
// every direction — anthropic gets {type:error, error:{type:
// invalid_request_error}}, the openai/responses family gets the
// unsupported_protocol_conversion code — always JSON with status 400 (no
// stream began, so stream:true requests get JSON too).
func TestWriteUnsupportedConversionError_ProtocolEnvelopes(t *testing.T) {
	err := &UnsupportedError{
		ClientProto: "openai", TargetProto: "anthropic",
		Feature: "audio", Detail: "Chat Completions input_audio content",
	}
	for _, proto := range []Protocol{OpenAI, Responses, Anthropic} {
		t.Run(string(proto), func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteUnsupportedConversionError(rec, proto, err)
			if rec.Code != http.StatusBadRequest || rec.Header().Get("content-type") != "application/json" {
				t.Fatalf("status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
			}
			body := unmarshalMap(t, rec.Body.Bytes())
			if proto == Anthropic {
				if body["type"] != "error" || asMap(body["error"])["type"] != "invalid_request_error" {
					t.Fatalf("anthropic envelope = %v", body)
				}
			} else if asMap(body["error"])["code"] != "unsupported_protocol_conversion" {
				t.Fatalf("openai envelope = %v", body)
			}
		})
	}
}
