package provider

// r6_quota_evidence_test.go pins the quota failure-evidence contract (R6
// review B4) for the providers that had not joined the shared evidence
// helper — codex wham/usage, deepseek user/balance and aqp monthly_usage —
// plus the aqp monthly_usage fetch behavior that moving the fetch onto the
// shared usage pipeline must preserve.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"model-proxy/internal/display"
)

// r6GatewayPage is a multi-line gateway error page in the shape of the
// 2026-11-18 aqp incident (an nginx page served for hours).
const r6GatewayPage = "<html>\n<head><title>503 Service Temporarily Unavailable</title></head>\n<body>\n<center><h1>503 Service Temporarily Unavailable</h1></center>\n<hr><center>nginx</center>\n</body>\n</html>\n"

// r6Evidence is the evidence text of body as the contract names it: the
// shared display.Truncate snippet (newline runs collapsed into one line,
// capped at 120 bytes with a trailing "..."), or the explicit "empty body".
// The cap itself is asserted without that helper by r6AssertEvidenceBounded.
func r6Evidence(body string) string {
	if body == "" {
		return "empty body"
	}
	return display.Truncate(body, 120)
}

// r6AssertEvidenceBounded checks the evidence of err — the text between head
// and tail — against the contract's bounds without going through
// display.Truncate: present, single-line, and at most 120 bytes plus the
// "..." marker. It returns the evidence.
func r6AssertEvidenceBounded(t *testing.T, err, head, tail string) string {
	t.Helper()
	if !strings.HasPrefix(err, head) || !strings.HasSuffix(err, tail) {
		t.Fatalf("error %q is not %q + evidence + %q", err, head, tail)
	}
	ev := strings.TrimSuffix(strings.TrimPrefix(err, head), tail)
	if ev == "" {
		t.Fatalf("error %q carries no evidence", err)
	}
	if strings.ContainsAny(ev, "\r\n") {
		t.Fatalf("evidence is not single-line: %q", ev)
	}
	if len(ev) > 120+len("...") {
		t.Fatalf("evidence is %d bytes, want at most 123 (120-byte cap + \"...\"): %q", len(ev), ev)
	}
	return ev
}

// r6JSONString returns s as JSON string content (newlines escaped), so a
// fixture envelope stays valid JSON while its message spans lines.
func r6JSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b[1 : len(b)-1])
}

// r6UnmarshalError returns encoding/json's own error text for body, so the
// expectations never hard-code the stdlib wording.
func r6UnmarshalError(t *testing.T, body string, target any) string {
	t.Helper()
	err := json.Unmarshal([]byte(body), target)
	if err == nil {
		t.Fatalf("fixture body unexpectedly parses into %T: %q", target, body)
	}
	return err.Error()
}

// r6ServeAnswers starts a server that answers every request with status and
// body, and returns its URL.
func r6ServeAnswers(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func r6CodexAt(url string) *CodexProvider {
	return &CodexProvider{cfg: &Config{OpenAIBaseURL: url + "/codex"}, auth: fakeAuth{key: "k"}}
}

func r6DeepseekAt(url string) *DeepSeekProvider {
	return &DeepSeekProvider{ApiKeyBase: NewApiKeyBaseWithKey("deepseek", "k"), cfg: &Config{UsageURL: url}}
}

func r6AqpAt(t *testing.T, url string) *AqpProvider {
	t.Helper()
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	writeAqpStore(t, store, "proj-1")
	return &AqpProvider{cfg: &Config{OAuthAuthFile: store, AqpBaseURL: url}}
}

// r6QuotaProvider is the slice of Provider the evidence tests drive.
type r6QuotaProvider interface {
	Quota() (*QuotaSnapshot, error)
}

// r6QuotaOf calls Quota() and enforces its contract: never a non-nil error.
func r6QuotaOf(t *testing.T, p r6QuotaProvider) *QuotaSnapshot {
	t.Helper()
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota() error = %v, want nil (contract: never a non-nil error)", err)
	}
	return s
}

// TestR6QuotaParseFailure_CarriesSingleLineBodyEvidence: a 200 whose body is
// not the provider's format must report a single-line error that carries a
// bounded snippet of the body — the evidence every other quota failure
// already has (a bare json error cannot tell a gateway HTML page from an
// empty body). codex and deepseek carried no body at all, aqp only the json
// error.
func TestR6QuotaParseFailure_CarriesSingleLineBodyEvidence(t *testing.T) {
	providers := []struct {
		name   string
		prefix string
		build  func(t *testing.T, url string) r6QuotaProvider
	}{
		{"codex wham/usage", "parse wham/usage: ", func(_ *testing.T, url string) r6QuotaProvider { return r6CodexAt(url) }},
		{"deepseek user/balance", "parse user/balance: ", func(_ *testing.T, url string) r6QuotaProvider { return r6DeepseekAt(url) }},
		{"aqp monthly_usage envelope", "monthly usage response parse failed: ", func(t *testing.T, url string) r6QuotaProvider {
			return r6AqpAt(t, url)
		}},
	}
	bodies := []struct{ name, body string }{
		{"gateway html page", r6GatewayPage},
		{"empty body", ""},
		{"truncated json", `{"plan_type":"pro","rate_limit":{"allowed":tr`},
	}
	for _, pc := range providers {
		for _, b := range bodies {
			t.Run(pc.name+"/"+b.name, func(t *testing.T) {
				s := r6QuotaOf(t, pc.build(t, r6ServeAnswers(t, http.StatusOK, b.body)))
				if s == nil {
					t.Fatal("Quota() returned a nil snapshot")
				}
				head := pc.prefix + r6UnmarshalError(t, b.body, new(any)) + ": "
				if s.Billing != BillingUnknown || s.Err != head+r6Evidence(b.body) {
					t.Fatalf("snapshot = %+v\nwant BillingUnknown Err=%q", s, head+r6Evidence(b.body))
				}
				r6AssertEvidenceBounded(t, s.Err, head, "")
			})
		}
	}
}

// TestR6AqpMonthlyUsage_DataParseFailureCarriesBodyEvidence: a valid
// envelope whose data is not the monthly usage object is the other aqp parse
// branch that reported the json error alone.
func TestR6AqpMonthlyUsage_DataParseFailureCarriesBodyEvidence(t *testing.T) {
	const body = `{"retcode":0,"data":[1,2,3]}`
	s := r6QuotaOf(t, r6AqpAt(t, r6ServeAnswers(t, http.StatusOK, body)))
	head := "monthly usage data parse failed: " + r6UnmarshalError(t, `[1,2,3]`, new(MonthlyProjectUsage)) + ": "
	if s == nil || s.Billing != BillingUnknown || s.Err != head+r6Evidence(body) {
		t.Fatalf("snapshot = %+v, want BillingUnknown Err=%q", s, head+r6Evidence(body))
	}
	r6AssertEvidenceBounded(t, s.Err, head, "")
}

// TestR6AqpMonthlyUsage_ParseFailuresKeepJSONErrorWrapped: appending the
// evidence must not replace the wrap — both parse failures keep encoding/json's
// error in their chain for callers that inspect it.
func TestR6AqpMonthlyUsage_ParseFailuresKeepJSONErrorWrapped(t *testing.T) {
	_, err := r6AqpAt(t, r6ServeAnswers(t, http.StatusOK, r6GatewayPage)).fetchMonthlyUsage()
	var syntaxErr *json.SyntaxError
	if err == nil || !errors.As(err, &syntaxErr) {
		t.Fatalf("envelope parse failure = %v, want a *json.SyntaxError in its chain", err)
	}
	_, err = r6AqpAt(t, r6ServeAnswers(t, http.StatusOK, `{"retcode":0,"data":[1,2,3]}`)).fetchMonthlyUsage()
	var typeErr *json.UnmarshalTypeError
	if err == nil || !errors.As(err, &typeErr) {
		t.Fatalf("data parse failure = %v, want a *json.UnmarshalTypeError in its chain", err)
	}
}

// TestR6AqpMonthlyUsage_Non200EvidenceIsSingleLineAndBounded: the non-200
// evidence is the envelope's message when present, the body otherwise — in
// both cases collapsed to one line and capped like every other evidence
// (the message used to ride along verbatim: unbounded and multi-line), and
// an empty body is named instead of leaving a dangling prefix.
func TestR6AqpMonthlyUsage_Non200EvidenceIsSingleLineAndBounded(t *testing.T) {
	const relogin = " - re-login: `model-proxy login aqp`"
	plain := func(status int) string { return fmt.Sprintf("monthly usage HTTP %d: ", status) }
	session := func(status int) string { return fmt.Sprintf("session expired (HTTP %d): ", status) }
	// An envelope message that itself spans lines and outgrows the evidence
	// window.
	longMessage := strings.Repeat("gateway maintenance window\n", 10) + "please retry later"
	cases := []struct {
		name   string
		status int
		body   string
		source string // the text shown as evidence
		head   string
		tail   string
	}{
		{"multi-line long envelope message", http.StatusBadGateway,
			`{"message":"` + r6JSONString(t, longMessage) + `"}`, longMessage, plain(http.StatusBadGateway), ""},
		{"session expired with a multi-line message", http.StatusUnauthorized,
			`{"message":"Session\nexpired, please re-login"}`, "Session\nexpired, please re-login",
			session(http.StatusUnauthorized), relogin},
		{"empty body", http.StatusServiceUnavailable, "", "", plain(http.StatusServiceUnavailable), ""},
		{"multi-line html page", http.StatusForbidden, r6GatewayPage, r6GatewayPage, plain(http.StatusForbidden), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := r6QuotaOf(t, r6AqpAt(t, r6ServeAnswers(t, tc.status, tc.body)))
			want := tc.head + r6Evidence(tc.source) + tc.tail
			if s == nil || s.Billing != BillingUnknown || s.Err != want {
				t.Fatalf("snapshot = %+v, want BillingUnknown Err=%q", s, want)
			}
			ev := r6AssertEvidenceBounded(t, s.Err, tc.head, tc.tail)
			// A source that outgrows the window is cut at exactly 120 bytes.
			if collapsed := strings.ReplaceAll(tc.source, "\n", " "); len(collapsed) > 120 {
				if len(ev) != 123 || !strings.HasSuffix(ev, "...") {
					t.Fatalf("evidence = %q (%d bytes), want the first 120 bytes + \"...\"", ev, len(ev))
				}
			}
		})
	}
}

// TestR6AqpMonthlyUsage_SessionProbeMatchesShownEvidence pins the scope of
// the "session" keyword probe on a non-200: it reads the SAME text that is
// shown as evidence — the capped, single-line snippet. The
// "session expired ... - re-login" wording is what CredentialErrClass (and
// the Web banner) key on, so the operator must be able to see the word that
// triggered it. A gateway page, or an envelope message, that merely mentions
// "session" past the evidence window is reported as the HTTP failure it is.
func TestR6AqpMonthlyUsage_SessionProbeMatchesShownEvidence(t *testing.T) {
	page := r6GatewayPage + "<!-- session store unavailable -->\n"
	if shown := r6Evidence(page); strings.Contains(strings.ToLower(shown), "session") {
		t.Fatalf("fixture: the evidence window already shows the word: %q", shown)
	}
	message := strings.Repeat("x", 200) + " session timed out"
	if shown := r6Evidence(message); strings.Contains(strings.ToLower(shown), "session") {
		t.Fatalf("fixture: the evidence window already shows the word: %q", shown)
	}
	cases := []struct {
		name   string
		status int
		body   string
		source string
	}{
		{"gateway page naming a session past the window", http.StatusBadGateway, page, page},
		{"envelope message naming a session past the window", http.StatusForbidden,
			`{"message":"` + r6JSONString(t, message) + `"}`, message},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := r6QuotaOf(t, r6AqpAt(t, r6ServeAnswers(t, tc.status, tc.body)))
			head := fmt.Sprintf("monthly usage HTTP %d: ", tc.status)
			want := head + r6Evidence(tc.source)
			if s == nil || s.Billing != BillingUnknown || s.Err != want {
				t.Fatalf("snapshot = %+v, want BillingUnknown Err=%q", s, want)
			}
			if strings.Contains(s.Err, "session expired") || strings.Contains(s.Err, "re-login") {
				t.Fatalf("a non-credential failure was worded as an expired session: %q", s.Err)
			}
			r6AssertEvidenceBounded(t, s.Err, head, "")
		})
	}
}

// TestR6AqpMonthlyUsage_RetcodeMessageIsEvidence: a non-zero retcode reports
// the envelope's message as the same evidence — one line, capped — instead
// of the verbatim upstream text.
func TestR6AqpMonthlyUsage_RetcodeMessageIsEvidence(t *testing.T) {
	const head = "monthly usage retcode=7 message="
	cases := []struct {
		name    string
		message string
	}{
		{"multi-line long message", strings.Repeat("quota exhausted on project\n", 8) + "top up"},
		{"empty message", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"retcode": 7, "message": tc.message})
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			s := r6QuotaOf(t, r6AqpAt(t, r6ServeAnswers(t, http.StatusOK, string(body))))
			want := head + r6Evidence(tc.message)
			if s == nil || s.Billing != BillingUnknown || s.Err != want {
				t.Fatalf("snapshot = %+v, want BillingUnknown Err=%q", s, want)
			}
			r6AssertEvidenceBounded(t, s.Err, head, "")
		})
	}
}

// r6SeenRequest is what the fake proxy observed of the monthly_usage call.
type r6SeenRequest struct {
	method, absoluteURL, cookie, contentType, authorization, body string
}

// TestR6AqpMonthlyUsage_FetchShapeAndProviderProxy pins the fetch the move
// onto the shared usage pipeline must preserve: a POST to monthly_usage
// carrying the SSO cookie and the project_id payload, sent through
// providers.<name>.proxy_url when it is set (an explicit proxy applies to
// loopback too) and direct otherwise.
func TestR6AqpMonthlyUsage_FetchShapeAndProviderProxy(t *testing.T) {
	const envelope = `{"retcode":0,"data":{"project_id":"proj-1","selected_year":2026,"selected_month":7,"total_amount":100,"usage":30,"balance":70,"plan":"CQP"}}`
	var directHits atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits.Add(1)
		_, _ = io.WriteString(w, envelope)
	}))
	defer direct.Close()

	var proxyHits atomic.Int32
	seen := make(chan r6SeenRequest, 4)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		seen <- r6SeenRequest{
			method:        r.Method,
			absoluteURL:   r.URL.String(),
			cookie:        r.Header.Get("Cookie"),
			contentType:   r.Header.Get("Content-Type"),
			authorization: r.Header.Get("Authorization"),
			body:          string(body),
		}
		_, _ = io.WriteString(w, envelope)
	}))
	defer proxy.Close()

	cases := []struct {
		name           string
		proxyURL       string
		wantProxyHits  int32
		wantDirectHits int32
	}{
		{"provider proxy_url set", proxy.URL, 1, 0},
		{"no override goes direct", "", 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			directBefore, proxyBefore := directHits.Load(), proxyHits.Load()
			store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
			writeAqpStore(t, store, "proj-1")
			p := &AqpProvider{cfg: &Config{OAuthAuthFile: store, AqpBaseURL: direct.URL, ProxyURL: tc.proxyURL}}
			mu, err := p.fetchMonthlyUsage()
			if err != nil {
				t.Fatalf("fetchMonthlyUsage: %v", err)
			}
			if mu == nil || mu.ProjectID != "proj-1" || mu.TotalAmount != 100 || mu.Balance != 70 || mu.Plan != "CQP" {
				t.Fatalf("monthly usage = %+v, want the envelope's data", mu)
			}
			proxySeen := proxyHits.Load() - proxyBefore
			if got := directHits.Load() - directBefore; got != tc.wantDirectHits {
				t.Errorf("direct endpoint hits = %d, want %d", got, tc.wantDirectHits)
			}
			if proxySeen != tc.wantProxyHits {
				t.Errorf("proxy hits = %d, want %d", proxySeen, tc.wantProxyHits)
			}
			if tc.wantProxyHits == 0 {
				return
			}
			// Never block on a regression that lost the proxied request: fail
			// instead of waiting for a record that will not come.
			var got r6SeenRequest
			select {
			case got = <-seen:
			default:
				t.Fatalf("the proxy counted %d hit(s) but recorded no request", proxySeen)
			}
			want := r6SeenRequest{
				method:      http.MethodPost,
				cookie:      "SSO_C=fake",
				contentType: "application/json",
				body:        `{"project_id":"proj-1"}`,
			}
			// A proxied plain-http request reaches the proxy in absolute form.
			want.absoluteURL = direct.URL + aqpMonthlyUsagePath
			if got != want {
				t.Errorf("request seen by the proxy = %+v, want %+v", got, want)
			}
		})
	}
}

// TestR6AqpMonthlyUsage_FailureMessages pins the failure wording the quota
// classifiers (IsTransientQuotaErr / CredentialErrClass and their JS twin)
// match on: the transport prefix, and the non-200 shapes with the status
// code, the body's message, and the re-login hint.
func TestR6AqpMonthlyUsage_FailureMessages(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	_, err := r6AqpAt(t, closed.URL).fetchMonthlyUsage()
	if err == nil || !strings.HasPrefix(err.Error(), "monthly usage request failed: ") {
		t.Fatalf("transport failure error = %v, want the %q prefix", err, "monthly usage request failed: ")
	}

	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"401 envelope message", http.StatusUnauthorized, `{"message":"Session expired"}`,
			"session expired (HTTP 401): Session expired - re-login: `model-proxy login aqp`"},
		{"401 without a message", http.StatusUnauthorized, "denied",
			"session expired (HTTP 401): denied - re-login: `model-proxy login aqp`"},
		{"5xx envelope message", http.StatusInternalServerError, `{"message":"boom"}`,
			"monthly usage HTTP 500: boom"},
		{"5xx plain body", http.StatusServiceUnavailable, "upstream unavailable",
			"monthly usage HTTP 503: upstream unavailable"},
		{"session keyword on a non-401 status", http.StatusForbidden, `{"message":"session timed out"}`,
			"session expired (HTTP 403): session timed out - re-login: `model-proxy login aqp`"},
		{"non-zero retcode", http.StatusOK, `{"retcode":1,"message":"denied"}`,
			"monthly usage retcode=1 message=denied"},
		{"missing data", http.StatusOK, `{"retcode":0}`,
			"monthly usage response missing data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r6AqpAt(t, r6ServeAnswers(t, tc.status, tc.body)).fetchMonthlyUsage()
			if err == nil || err.Error() != tc.want {
				t.Fatalf("fetchMonthlyUsage error = %v, want %q", err, tc.want)
			}
			s := r6QuotaOf(t, r6AqpAt(t, r6ServeAnswers(t, tc.status, tc.body)))
			if s == nil || s.Billing != BillingUnknown || s.Err != tc.want {
				t.Fatalf("Quota() = %+v, want BillingUnknown carrying the same error text", s)
			}
		})
	}
}
