package targetexec

import (
	"net/http/httptest"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
)

func TestNewAttemptPreservesFiveTypedGroups(t *testing.T) {
	cache := responsecache.New(responsecache.Options{
		TTL: time.Minute, MaxEntries: 1, MaxBodyBytes: 16,
	})
	request := httptest.NewRequest("POST", "/v1/responses", nil)
	writer := httptest.NewRecorder()
	body := []byte(`{"model":"client"}`)
	retry := configdomain.RouteTarget{Provider: "larger", Model: "model"}

	attempt := NewAttempt(
		Runtime{
			Scheduling: configdomain.Scheduling{UpstreamTimeout: "3s"},
			Generation: 17,
			Cache:      cache,
		},
		Plan{},
		Exchange{Request: request, Writer: writer, Body: body},
		Scope{
			CalledModel: "client",
			Agent:       "codex",
			CacheKey:    "cache-key",
			Log: LogContext{
				RequestID: "req-1", Attempt: 2, Exposed: "public", OriginalBody: body,
			},
			ResponsesHistory: []any{"history"},
			ResponsesSession: "session",
		},
		Policy{
			Force: true, LastTarget: true,
			ContextRetry: func() []configdomain.RouteTarget {
				return []configdomain.RouteTarget{retry}
			},
		},
	)

	if got := attempt.Runtime(); got.Generation != 17 || got.Cache != cache || got.Scheduling.Timeout() != 3*time.Second {
		t.Fatalf("runtime = %+v", got)
	}
	if got := attempt.Exchange(); got.Request != request || got.Writer != writer || string(got.Body) != string(body) {
		t.Fatalf("exchange = %+v", got)
	}
	if got := attempt.Scope(); got.CalledModel != "client" || got.Agent != "codex" ||
		got.Log.RequestID != "req-1" || got.ResponsesSession != "session" {
		t.Fatalf("scope = %+v", got)
	}
	if got := attempt.Policy(); !got.Force || !got.LastTarget ||
		len(got.ContextRetry()) != 1 || got.ContextRetry()[0] != retry {
		t.Fatalf("policy = %+v", got)
	}
	if string(attempt.Exchange().Body) != `{"model":"client"}` {
		t.Fatalf("constructor changed prepared body: %s", attempt.Exchange().Body)
	}
}

func TestCommitExposesOnlyPreparedRequestBody(t *testing.T) {
	body := []byte(`{"model":"upstream"}`)
	commit := NewCommit(body)
	if string(commit.RequestBody()) != string(body) {
		t.Fatalf("commit body = %s", commit.RequestBody())
	}
	var nilCommit *Commit
	if nilCommit.RequestBody() != nil {
		t.Fatal("nil commit returned a body")
	}
}
