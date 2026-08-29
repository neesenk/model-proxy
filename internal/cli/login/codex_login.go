package login

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"model-proxy/internal/provider"
)

// codex OAuth device flow (independent tokens, not shared with codex CLI).
// Contract from codex-rs/login source:
//
//	issuer = https://auth.openai.com, client_id = app_EMoamEEZ73f0CkXaXp7hrann
//	1. POST {issuer}/api/accounts/deviceauth/usercode  {client_id} → {device_auth_id, user_code, interval}
//	2. user visits {issuer}/codex/device and enters user_code
//	3. POST {issuer}/api/accounts/deviceauth/token  {device_auth_id, user_code} (poll every interval)
//	   → {authorization_code, code_challenge, code_verifier}  (or pending/slow_down)
//	4. POST {issuer}/oauth/token  form: grant_type=authorization_code&code=&redirect_uri={issuer}/deviceauth/callback&client_id=&code_verifier=
//	   → {id_token, access_token, refresh_token}
const (
	CodexOAuthIssuer    = "https://auth.openai.com"
	CodexOAuthUsercode  = CodexOAuthIssuer + "/api/accounts/deviceauth/usercode"
	CodexOAuthDeviceTok = CodexOAuthIssuer + "/api/accounts/deviceauth/token"
	CodexOAuthCallback  = CodexOAuthIssuer + "/deviceauth/callback"
	CodexOAuthVerifyURL = CodexOAuthIssuer + "/codex/device"
)

// CodexLoginServerOptions holds overrides for testing.
type CodexLoginServerOptions struct {
	UsercodeURL  string
	DeviceTokURL string
	TokenURL     string // oauth/token (exchange + refresh)
	HTTPClient   *http.Client
}

func (o *CodexLoginServerOptions) Defaults() {
	if o.UsercodeURL == "" {
		o.UsercodeURL = CodexOAuthUsercode
	}
	if o.DeviceTokURL == "" {
		o.DeviceTokURL = CodexOAuthDeviceTok
	}
	if o.TokenURL == "" {
		o.TokenURL = provider.CodexOAuthTokenURL
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
}

// UserCodeResponse is the response from deviceauth/usercode.
// interval comes as a JSON string (e.g. "5"), per codex-rs's custom deserializer.
type UserCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	Interval     string `json:"interval"`
}

// CodeSuccessResponse is the success response from deviceauth/token polling.
type CodeSuccessResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

func RequestUserCode(opts *CodexLoginServerOptions, clientID string) (*UserCodeResponse, error) {
	return RequestUserCodeContext(context.Background(), opts, clientID)
}

// requestUserCodeContext is requestUserCode with caller-controlled cancellation
// of the device-flow bootstrap HTTP request.
func RequestUserCodeContext(ctx context.Context, opts *CodexLoginServerOptions, clientID string) (*UserCodeResponse, error) {
	body, _ := json.Marshal(map[string]string{"client_id": clientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.UsercodeURL, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("request user code request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request user code: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("request user code: HTTP %d: %s", resp.StatusCode, provider.Truncate(string(rb), 200))
	}
	var uc UserCodeResponse
	if err := json.Unmarshal(rb, &uc); err != nil {
		return nil, fmt.Errorf("parse user code response: %w", err)
	}
	if uc.DeviceAuthID == "" || uc.UserCode == "" {
		return nil, fmt.Errorf("user code response missing fields: %s", provider.Truncate(string(rb), 200))
	}
	return &uc, nil
}

// devicePollErrorCode extracts the error code from a deviceauth/token error
// response. Handles both nested {"error":{"code":...}} (real OpenAI) and flat
// {"error":"..."} (test mocks).
func DevicePollErrorCode(body []byte) string {
	var nested struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &nested); err == nil && nested.Error.Code != "" {
		return nested.Error.Code
	}
	var flat struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &flat); err == nil {
		return flat.Error
	}
	return ""
}

// pollForToken polls deviceauth/token until the user authorizes (or 15min timeout).
// Returns the authorization_code + code_verifier on success.
func PollForToken(opts *CodexLoginServerOptions, deviceAuthID, userCode string, interval int) (*CodeSuccessResponse, error) {
	return PollForTokenContext(context.Background(), opts, deviceAuthID, userCode, interval)
}

// pollForTokenContext is pollForToken with caller-controlled cancellation. The
// context covers both each device-token HTTP request and the wait between polls.
func PollForTokenContext(ctx context.Context, opts *CodexLoginServerOptions, deviceAuthID, userCode string, interval int) (*CodeSuccessResponse, error) {
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("poll device token canceled: %w", err)
		}
		body, _ := json.Marshal(map[string]string{
			"device_auth_id": deviceAuthID,
			"user_code":      userCode,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.DeviceTokURL, strings.NewReader(string(body)))
		if err != nil {
			return nil, fmt.Errorf("poll device token request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		resp, err := opts.HTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("poll device token: %w", err)
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			var cs CodeSuccessResponse
			if err := json.Unmarshal(rb, &cs); err != nil {
				return nil, fmt.Errorf("parse device token response: %w", err)
			}
			if cs.AuthorizationCode != "" {
				return &cs, nil
			}
		}
		// Non-200: parse error to check pending/slow_down/expired.
		// Real OpenAI format: {"error":{"code":"deviceauth_authorization_pending",...}}
		// Test mocks use flat {"error":"pending"} — handle both.
		code := DevicePollErrorCode(rb)
		switch code {
		case "deviceauth_authorization_pending", "pending":
			// keep polling
		case "deviceauth_slow_down", "slow_down":
			interval += 5
		case "deviceauth_authorization_expired", "expired_token":
			return nil, fmt.Errorf("device code expired; run `model-proxy login codex` again")
		case "deviceauth_authorization_denied", "access_denied":
			return nil, fmt.Errorf("user denied the authorization")
		default:
			return nil, fmt.Errorf("device token poll: HTTP %d: %s", resp.StatusCode, provider.Truncate(string(rb), 200))
		}

		timer := time.NewTimer(time.Duration(interval) * time.Second)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("poll device token canceled: %w", ctx.Err())
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("device login timed out after 15 minutes")
}

// exchangeCodeForTokens trades the authorization_code for access/refresh/id tokens.
func ExchangeCodeForTokens(opts *CodexLoginServerOptions, clientID, authCode, codeVerifier string) (*provider.CodexAuthFile, error) {
	return ExchangeCodeForTokensContext(context.Background(), opts, clientID, authCode, codeVerifier)
}

// exchangeCodeForTokensContext is exchangeCodeForTokens with caller-controlled
// cancellation of the token-exchange HTTP request.
func ExchangeCodeForTokensContext(ctx context.Context, opts *CodexLoginServerOptions, clientID, authCode, codeVerifier string) (*provider.CodexAuthFile, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {CodexOAuthCallback},
		"client_id":     {clientID},
		"code_verifier": {codeVerifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("exchange code request: %w", err)
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("exchange code: HTTP %d: %s", resp.StatusCode, provider.Truncate(string(rb), 200))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(rb, &tok); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token: %s", provider.Truncate(string(rb), 200))
	}
	af := &provider.CodexAuthFile{AuthMode: "chatgpt"}
	af.Tokens.AccessToken = tok.AccessToken
	af.Tokens.RefreshToken = tok.RefreshToken
	af.Tokens.IDToken = tok.IDToken
	af.Tokens.AccountID = provider.AccountIDFromTokens(tok.IDToken, "")
	af.LastRefresh = time.Now().UTC().Format(time.RFC3339Nano)
	return af, nil
}

// runCodexLoginFlow performs the interactive device flow and persists tokens
// to authFile. Errors are returned (no process exit), so non-CLI orchestrators
// (the `add` command) can drive the same flow. The caller derives authFile from
// the config top-level key (NOT the provider_id "codex") via oauthAuthFilePath,
// so credentials land in <provName>_oauth_auth.json — the same path the forward
// path / web UI / logout read (a renamed instance like "codex-work" writes
// codex-work_oauth_auth.json).
func runCodexLoginFlow(authFile string) error {
	opts := &CodexLoginServerOptions{}
	opts.Defaults()

	fmt.Println("Requesting device code from OpenAI...")
	uc, err := RequestUserCode(opts, provider.CodexOAuthClientID)
	if err != nil {
		return err
	}
	fmt.Printf("\n  Open this URL: %s\n", CodexOAuthVerifyURL)
	fmt.Printf("  Enter code:   %s\n\n", uc.UserCode)
	fmt.Println("Waiting for authorization (15 min timeout)...")

	interval, _ := strconv.Atoi(uc.Interval)
	cs, err := PollForToken(opts, uc.DeviceAuthID, uc.UserCode, interval)
	if err != nil {
		return err
	}
	fmt.Println("Authorized. Exchanging code for tokens...")
	af, err := ExchangeCodeForTokens(opts, provider.CodexOAuthClientID, cs.AuthorizationCode, cs.CodeVerifier)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(DirOf(authFile), 0o700); err != nil {
		return err
	}
	if err := provider.WriteCodexAuthFile(authFile, af); err != nil {
		return err
	}
	fmt.Printf("✓ codex OAuth tokens saved to %s\n", authFile)
	if af.Tokens.AccountID != "" {
		fmt.Printf("  account_id: %s\n", af.Tokens.AccountID)
	}
	fmt.Println("\nYou can now use codex-native models (gpt-5.5) through the proxy.")
	return nil
}

// dirOf returns the directory of a path, or "." if none.
func DirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}
