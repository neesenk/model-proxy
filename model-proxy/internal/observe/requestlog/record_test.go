package requestlog

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBuildRecordPreservesFieldsAndExactHeaderAllowlist(t *testing.T) {
	const originalBody = "{\"model\":\"client-original\"}\n"
	const responseBody = "event: done\ndata: {}\n\n"
	timestamp := time.Date(2026, 7, 28, 9, 8, 7, 0, time.FixedZone("UTC+8", 8*60*60))
	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	header.Set("X-Request-Id", "upstream-rid")
	header.Set("Retry-After", "7")
	header.Set("Authorization", "Bearer must-not-leak")
	header.Set("Set-Cookie", "session=must-not-leak")
	header.Set("X-Api-Key", "must-not-leak")
	header.Set("X-Unlisted", "must-not-leak")

	logger := New(Options{Directory: t.TempDir(), MaxBodyBytes: 1 << 20})
	record := logger.BuildRecord(Input{
		Timestamp:         timestamp,
		StartedAt:         timestamp.Add(-77 * time.Millisecond),
		RequestID:         "shadow-primary-rid",
		SessionID:         "session-1",
		Protocol:          "anthropic",
		Method:            "POST",
		Path:              "/v1/messages",
		CalledModel:       "client-model",
		UpstreamModel:     "upstream-model",
		Exposed:           "route-model",
		Provider:          "provider-a",
		Attempt:           2,
		Status:            201,
		RequestBody:       []byte(originalBody),
		ResponseBody:      []byte(responseBody),
		ResponseSize:      int64(len(responseBody)),
		ResponseHeader:    header,
		ResponseTruncated: false,
	})

	if record.Ts != "2026-07-28T01:08:07Z" {
		t.Errorf("Ts = %q, want UTC timestamp", record.Ts)
	}
	if !record.Shadow {
		t.Error("shadow- request id did not set Shadow")
	}
	if record.RequestID != "shadow-primary-rid" ||
		record.SessionID != "session-1" ||
		record.Protocol != "anthropic" ||
		record.Method != "POST" ||
		record.Path != "/v1/messages" ||
		record.CalledModel != "client-model" ||
		record.UpstreamModel != "upstream-model" ||
		record.Exposed != "route-model" ||
		record.Provider != "provider-a" {
		t.Errorf("string fields were not preserved: %+v", record)
	}
	if record.Attempt != 2 || record.Status != 201 || record.LatencyMs != 77 {
		t.Errorf("attempt/status/latency = %d/%d/%d, want 2/201/77",
			record.Attempt, record.Status, record.LatencyMs)
	}
	if record.RequestSize != len(originalBody) || record.RequestBody != originalBody {
		t.Errorf("request size/body = %d/%q, want %d/%q",
			record.RequestSize, record.RequestBody, len(originalBody), originalBody)
	}
	if record.ResponseSize != int64(len(responseBody)) || record.ResponseBody != responseBody {
		t.Errorf("response size/body = %d/%q, want %d/%q",
			record.ResponseSize, record.ResponseBody, len(responseBody), responseBody)
	}
	const wantHeaders = `{"content-type":"text/event-stream","retry-after":"7","x-request-id":"upstream-rid"}`
	if record.ResponseHeaders != wantHeaders {
		t.Errorf("ResponseHeaders = %s, want exact allowlist %s", record.ResponseHeaders, wantHeaders)
	}
	for _, secret := range []string{"authorization", "cookie", "api-key", "unlisted", "must-not-leak"} {
		if strings.Contains(strings.ToLower(record.ResponseHeaders), secret) {
			t.Errorf("ResponseHeaders leaked %q: %s", secret, record.ResponseHeaders)
		}
	}

	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(line, []byte{'\n'}) {
		t.Errorf("marshaled record contains a literal newline: %q", line)
	}
}

func TestBuildRecordTruncationPreservesOriginalSizes(t *testing.T) {
	const maxBody = 8
	requestBody := []byte("0123456789ab")
	capturedResponse := []byte("abcdefgh")
	logger := New(Options{Directory: t.TempDir(), MaxBodyBytes: maxBody})
	record := logger.BuildRecord(Input{
		Timestamp:         time.Date(2026, 7, 28, 1, 2, 3, 0, time.UTC),
		RequestID:         "primary-rid",
		RequestBody:       requestBody,
		ResponseBody:      capturedResponse,
		ResponseSize:      99,
		ResponseTruncated: true,
	})

	if record.Shadow {
		t.Error("non-shadow request id set Shadow")
	}
	if record.RequestSize != len(requestBody) {
		t.Errorf("RequestSize = %d, want original size %d", record.RequestSize, len(requestBody))
	}
	if want := "01234567" + truncationMarker; record.RequestBody != want {
		t.Errorf("RequestBody = %q, want %q", record.RequestBody, want)
	}
	if record.ResponseSize != 99 {
		t.Errorf("ResponseSize = %d, want original total 99", record.ResponseSize)
	}
	if want := string(capturedResponse) + truncationMarker; record.ResponseBody != want {
		t.Errorf("ResponseBody = %q, want %q", record.ResponseBody, want)
	}
	if !record.RequestBodyTruncated() {
		t.Error("RequestBodyTruncated did not recognize the request marker")
	}
	if (Record{RequestBody: "prefix " + truncationMarker + " suffix"}).RequestBodyTruncated() ||
		(Record{RequestBody: "complete body"}).RequestBodyTruncated() {
		t.Error("RequestBodyTruncated accepted a non-suffix or complete body")
	}
}

func TestBuildRecordOmitsResponseHeadersWithoutAllowedFields(t *testing.T) {
	logger := New(Options{Directory: t.TempDir(), MaxBodyBytes: 1024})
	record := logger.BuildRecord(Input{
		Timestamp:      time.Date(2026, 7, 28, 1, 2, 3, 0, time.UTC),
		RequestID:      "rid",
		ResponseHeader: http.Header{"Authorization": {"Bearer secret"}, "Set-Cookie": {"secret=1"}},
	})
	if record.ResponseHeaders != "" {
		t.Errorf("ResponseHeaders = %q, want empty when no allowlisted fields exist", record.ResponseHeaders)
	}
}
