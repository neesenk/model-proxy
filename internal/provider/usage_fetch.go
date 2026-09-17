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

// quotaHTTPClient is the shared client for usage/quota GETs: 30s budget, and
// the automatic proxy chain (env → system → direct) used by every
// non-forwarding outbound call.
func quotaHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: upstreamproxy.AutoTransport()}
}

// usageGet issues the usage/quota GET shared by the billing providers:
// provider auth first, then the configured extra headers, via the shared
// quota client. ok == false means fail carries the failure as a
// BillingUnknown snapshot and the caller must return it (never a non-nil
// error — the Quota() contract). A non-200 status is formatted by statusHint
// (nil → bare "HTTP %d").
func usageGet(url string, auth func(*http.Request) error, headers map[string]string, statusHint func(int) string) (body []byte, fail *QuotaSnapshot, ok bool) {
	req, _ := http.NewRequest("GET", url, nil)
	if err := auth(req); err != nil {
		return nil, &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := quotaHTTPClient().Do(req)
	if err != nil {
		return nil, &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, false
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		hint := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if statusHint != nil {
			hint = statusHint(resp.StatusCode)
		}
		return nil, &QuotaSnapshot{Billing: BillingUnknown, Err: hint}, false
	}
	return body, nil, true
}

// usageGetForDisplay is the CLI `usage` counterpart of usageGet: failures are
// printed in the shared CLI shape (not-logged-in hint on auth failure, red
// error lines otherwise) and ok == false means the caller must return nil.
// loginName completes the "Run: model-proxy login <loginName>" hint.
func usageGetForDisplay(url, loginName string, auth func(*http.Request) error, headers map[string]string) (body []byte, ok bool) {
	req, _ := http.NewRequest("GET", url, nil)
	if err := auth(req); err != nil {
		fmt.Println(display.Yellow("Not logged in.") + " Run: " + display.Cyan("model-proxy login "+loginName))
		return nil, false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := quotaHTTPClient().Do(req)
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

// bigmodelQuota fetches and parses the BigModel quota envelope shared by
// zhipu and zcode (zcode IS BigModel — same envelope). Follows the Quota()
// failure contract via usageGet.
func bigmodelQuota(url string, auth func(*http.Request) error, headers map[string]string) (*QuotaSnapshot, error) {
	body, fail, ok := usageGet(url, auth, headers, nil)
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
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "not zhipu quota format"}, nil
	}
	return s, nil
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
