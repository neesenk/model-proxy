package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"model-proxy/internal/display"
	"model-proxy/internal/upstreamproxy"
)

// usage_fetch.go holds the shared usage/quota GET boilerplate for the billing
// providers. Two flavors with distinct failure contracts:
//
//   - usageGet (Quota path): failures become a BillingUnknown snapshot,
//     never a non-nil error, so the scheduler poll stays alive;
//   - usageGetForDisplay (CLI Usage path): failures print the shared CLI
//     error lines and the caller returns nil.
//
// usageSend is the request-execution core shared by usageGet and by aqp's
// POST monthly_usage (a body to send and its own non-200 wording).
// usageGetForDisplay still issues its own request: it prints auth failures
// and transport failures as different CLI lines, which one merged error
// would hide.

// quotaHTTPClient is the shared client for usage/quota GETs: 30s budget, and
// the automatic proxy chain (env → system → direct) used by every
// non-forwarding outbound call.
func quotaHTTPClient(proxyURL string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: upstreamproxy.ProviderTransport(proxyURL)}
}

// usageSend issues one prepared usage/quota request through the shared quota
// client: provider auth first, then the configured extra headers. It returns
// the response body with its status; the caller owns the non-200 wording
// (usageGet's statusHint snapshot, aqp's session-expired classification), so
// a non-200 status is not an error here.
func usageSend(req *http.Request, proxyURL string, auth func(*http.Request) error, headers map[string]string) (body []byte, status int, err error) {
	if err := auth(req); err != nil {
		return nil, 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := quotaHTTPClient(proxyURL).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// usageGet issues the usage/quota GET shared by the billing providers:
// provider auth first, then the configured extra headers, via the shared
// quota client. ok == false means fail carries the failure as a
// BillingUnknown snapshot and the caller must return it (never a non-nil
// error — the Quota() contract). A non-200 status is formatted by statusHint
// (nil → bare "HTTP %d").
func usageGet(url, proxyURL string, auth func(*http.Request) error, headers map[string]string, statusHint func(int) string) (body []byte, fail *QuotaSnapshot, ok bool) {
	req, _ := http.NewRequest("GET", url, nil)
	body, status, err := usageSend(req, proxyURL, auth, headers)
	if err != nil {
		return nil, &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, false
	}
	if status != 200 {
		hint := fmt.Sprintf("HTTP %d", status)
		if statusHint != nil {
			hint = statusHint(status)
		}
		return nil, &QuotaSnapshot{Billing: BillingUnknown, Err: hint}, false
	}
	return body, nil, true
}

// usageGetForDisplay is the CLI `usage` counterpart of usageGet: failures are
// printed in the shared CLI shape (not-logged-in hint on auth failure, red
// error lines otherwise) and ok == false means the caller must return nil.
// loginName completes the "Run: model-proxy login <loginName>" hint.
func usageGetForDisplay(url, proxyURL, loginName string, auth func(*http.Request) error, headers map[string]string) (body []byte, ok bool) {
	req, _ := http.NewRequest("GET", url, nil)
	if err := auth(req); err != nil {
		fmt.Println(display.Yellow("Not logged in.") + " Run: " + display.Cyan("model-proxy login "+loginName))
		return nil, false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := quotaHTTPClient(proxyURL).Do(req)
	if err != nil {
		fmt.Println(display.Red("Error: usage request: " + err.Error()))
		return nil, false
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", display.Red("Error:"), resp.StatusCode, display.Truncate(string(body), 200))
		return nil, false
	}
	return body, true
}

// bigmodelQuota fetches and parses the BigModel quota envelope. Follows the
// Quota() failure contract via usageGet.
func bigmodelQuota(url, proxyURL string, auth func(*http.Request) error, headers map[string]string) (*QuotaSnapshot, error) {
	body, fail, ok := usageGet(url, proxyURL, auth, headers, nil)
	if !ok {
		return fail, nil
	}
	s, _ := ParseZhipuQuota(body, "")
	if s == nil {
		// Distinguish the BigModel error envelope ({"success":false,"code":…,
		// "msg":…} — e.g. their quota endpoint serving 内部服务器错误 after a
		// backend incident) from a body that simply isn't this API at all: the
		// former must surface the upstream message instead of the misleading
		// "not zhipu quota format".
		if msg := bigmodelErrorMessage(body); msg != "" {
			return &QuotaSnapshot{Billing: BillingUnknown, Err: "zhipu quota upstream error: " + msg}, nil
		}
		return quotaFormatSnapshot("not zhipu quota format", body), nil
	}
	return s, nil
}

// quotaBodyEvidence is the response-body evidence of the quota PARSE
// failures: quotaFormatSnapshot (the "not X format" and parse-failure
// snapshots of zhipu/zcode, kimi-code, opencode-go, codex and deepseek) and
// aqp's monthly_usage upstream texts (non-200 status, non-zero retcode,
// parse failures) read it. It is a truncated, single-line snippet
// (display.Truncate collapses newline runs, so a gateway/WAF HTML page can't
// spill as unprefixed log lines — the 2026-11-18 aqp incident). An empty body
// is called out explicitly (the 2026-11-19 kimi-code /usages flap: one 401,
// then fifteen 200-with-foreign-body failures, whose bare format string was
// unattributable and whose empty bodies produced a dangling "format: "
// suffix).
//
// It is not the evidence of every quota failure: usageGet's non-200 result
// is the status hint alone (statusHint sees no body), usageGetForDisplay
// prints its own CLI lines, and the BigModel error-envelope branch surfaces
// the upstream message instead of the body.
func quotaBodyEvidence(body []byte) string {
	snippet := display.Truncate(string(body), 120)
	if snippet == "" {
		snippet = "empty body"
	}
	return snippet
}

// quotaFormatSnapshot builds the "not X format" / parse failure as a
// BillingUnknown snapshot with the response body as evidence
// (quotaBodyEvidence), so the failure text can tell a gateway page, an error
// envelope and an empty body apart.
func quotaFormatSnapshot(formatErr string, body []byte) *QuotaSnapshot {
	return &QuotaSnapshot{Billing: BillingUnknown, Err: formatErr + ": " + quotaBodyEvidence(body)}
}

// bigmodelErrorMessage extracts the message from a BigModel error envelope
// ({"success":false, "code":…, "msg":"…"}), "" for anything else.
func bigmodelErrorMessage(body []byte) string {
	var env struct {
		Success *bool  `json:"success"`
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Success == nil || *env.Success || env.Msg == "" {
		return ""
	}
	return fmt.Sprintf("code %d: %s", env.Code, env.Msg)
}
