package targetexec

import (
	"encoding/json"
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

// TestDeveloperRoleRejectionDetection covers the observed 400 wordings of
// chat upstreams that predate the OpenAI developer role.
func TestDeveloperRoleRejectionDetection(t *testing.T) {
	volc := "{\"code\":\"InvalidParameter\",\"message\":\"The parameter `messages.role` specified in the request are not valid: invalid value: `developer`, supported values are [system user assistant tool]\"}"
	deepseek := "{\"message\":\"Failed to deserialize the JSON body into the target type: messages[0].role: unknown variant `developer`, expected one of `system`, `user`\"}"
	kimi := "{\"message\":\"role 'developer' is not a valid role\"}"
	zhipu := "{\"code\":\"1214\",\"message\":\"角色信息不正确\"}"
	for name, body := range map[string]string{
		"volcengine invalid value": volc,
		"deepseek unknown variant": deepseek,
		"kimi not a valid role":    kimi,
		"zhipu generic role":       zhipu,
	} {
		if !IsDeveloperRoleRejection(400, []byte(body)) {
			t.Errorf("%s: not detected", name)
		}
	}
	for name, body := range map[string]string{
		"param not supported": "Unsupported parameter: 'max_tokens' is not supported",
		"model not found":     "Model not found: m1",
		"empty":               "",
	} {
		if IsDeveloperRoleRejection(400, []byte(body)) {
			t.Errorf("%s: false positive", name)
		}
	}
	if IsDeveloperRoleRejection(500, []byte(kimi)) {
		t.Error("non-400 status must not trigger")
	}
}

// TestRenameDeveloperRole pins the rename: developer→system, everything else
// (other roles, top-level params, string content) preserved byte-for-value.
func TestRenameDeveloperRole(t *testing.T) {
	in := []byte(`{"model":"m","stream":true,"messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`)
	out, changed := RenameDeveloperRole(in)
	if !changed {
		t.Fatal("changed=false")
	}
	var v struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("renamed body not valid JSON: %v", err)
	}
	if v.Model != "m" || !v.Stream {
		t.Errorf("top-level fields lost: %s", out)
	}
	if v.Messages[0].Role != "system" || v.Messages[0].Content != "sys" {
		t.Errorf("developer message not renamed in place: %s", out)
	}
	if v.Messages[1].Role != "user" {
		t.Errorf("other roles touched: %s", out)
	}

	if _, changed := RenameDeveloperRole([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); changed {
		t.Error("body without developer role must pass through unchanged")
	}
	if _, changed := RenameDeveloperRole([]byte(`not json`)); changed {
		t.Error("non-JSON body must pass through unchanged")
	}
}

// TestThinkingDialectRejectionAndRewrite pins the adaptive-thinking lesson:
// shopee's wording is detected, budget thinking is rewritten to adaptive, and
// already-adaptive or thinking-less bodies pass through unchanged.
func TestThinkingDialectRejectionAndRewrite(t *testing.T) {
	shopee := `{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.enabled\" is not supported for this model. Use \"thinking.type.adaptive\""}}`
	if !IsThinkingDialectRejection(400, []byte(shopee)) {
		t.Error("shopee wording not detected")
	}
	if IsThinkingDialectRejection(400, []byte("Unsupported parameter: 'temperature' is not supported")) {
		t.Error("param rejection misclassified as thinking dialect")
	}
	if IsThinkingDialectRejection(500, []byte(shopee)) {
		t.Error("non-400 must not trigger")
	}

	in := []byte(`{"model":"m","thinking":{"type":"enabled","budget_tokens":8000},"messages":[{"role":"user","content":"hi"}]}`)
	out, changed := RewriteThinkingAdaptive(in)
	if !changed {
		t.Fatal("changed=false")
	}
	var v struct {
		Thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
		Messages []any `json:"messages"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("rewritten body invalid: %v", err)
	}
	if v.Thinking.Type != "adaptive" || v.Thinking.BudgetTokens != 0 {
		t.Errorf("thinking not adaptive: %s", out)
	}
	if len(v.Messages) != 1 {
		t.Errorf("messages lost: %s", out)
	}
	if _, changed := RewriteThinkingAdaptive([]byte(`{"thinking":{"type":"adaptive"}}`)); changed {
		t.Error("already-adaptive body must pass unchanged")
	}
	if _, changed := RewriteThinkingAdaptive([]byte(`{"messages":[]}`)); changed {
		t.Error("body without thinking must pass unchanged")
	}
}
