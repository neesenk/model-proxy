package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// codex OAuth device flow (independent tokens, not shared with codex CLI).
// Contract from codex-rs/login source:
//   issuer = https://auth.openai.com, client_id = app_EMoamEEZ73f0CkXaXp7hrann
//   1. POST {issuer}/api/accounts/deviceauth/usercode  {client_id} → {device_auth_id, user_code, interval}
//   2. user visits {issuer}/codex/device and enters user_code
//   3. POST {issuer}/api/accounts/deviceauth/token  {device_auth_id, user_code} (poll every interval)
//      → {authorization_code, code_challenge, code_verifier}  (or pending/slow_down)
//   4. POST {issuer}/oauth/token  form: grant_type=authorization_code&code=&redirect_uri={issuer}/deviceauth/callback&client_id=&code_verifier=
//      → {id_token, access_token, refresh_token}
const (
	codexOAuthIssuer    = "https://auth.openai.com"
	codexOAuthUsercode  = codexOAuthIssuer + "/api/accounts/deviceauth/usercode"
	codexOAuthDeviceTok = codexOAuthIssuer + "/api/accounts/deviceauth/token"
	codexOAuthCallback  = codexOAuthIssuer + "/deviceauth/callback"
	codexOAuthVerifyURL = codexOAuthIssuer + "/codex/device"
)

// codexLoginServerOptions holds overrides for testing.
type codexLoginServerOptions struct {
	usercodeURL  string
	deviceTokURL string
	tokenURL     string // oauth/token (exchange + refresh)
	httpClient   *http.Client
}

func (o *codexLoginServerOptions) defaults() {
	if o.usercodeURL == "" {
		o.usercodeURL = codexOAuthUsercode
	}
	if o.deviceTokURL == "" {
		o.deviceTokURL = codexOAuthDeviceTok
	}
	if o.tokenURL == "" {
		o.tokenURL = codexOAuthTokenURL
	}
	if o.httpClient == nil {
		o.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
}

// userCodeResponse is the response from deviceauth/usercode.
// interval comes as a JSON string (e.g. "5"), per codex-rs's custom deserializer.
type userCodeResponse struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
	Interval     string `json:"interval"`
}

// codeSuccessResponse is the success response from deviceauth/token polling.
type codeSuccessResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

func requestUserCode(opts *codexLoginServerOptions, clientID string) (*userCodeResponse, error) {
	body, _ := json.Marshal(map[string]string{"client_id": clientID})
	req, _ := http.NewRequest(http.MethodPost, opts.usercodeURL, strings.NewReader(string(body)))
	req.Header.Set("content-type", "application/json")
	resp, err := opts.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request user code: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("request user code: HTTP %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var uc userCodeResponse
	if err := json.Unmarshal(rb, &uc); err != nil {
		return nil, fmt.Errorf("parse user code response: %w", err)
	}
	if uc.DeviceAuthID == "" || uc.UserCode == "" {
		return nil, fmt.Errorf("user code response missing fields: %s", truncate(string(rb), 200))
	}
	return &uc, nil
}

// devicePollErrorCode extracts the error code from a deviceauth/token error
// response. Handles both nested {"error":{"code":...}} (real OpenAI) and flat
// {"error":"..."} (test mocks).
func devicePollErrorCode(body []byte) string {
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
func pollForToken(opts *codexLoginServerOptions, deviceAuthID, userCode string, interval int) (*codeSuccessResponse, error) {
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		body, _ := json.Marshal(map[string]string{
			"device_auth_id": deviceAuthID,
			"user_code":      userCode,
		})
		req, _ := http.NewRequest(http.MethodPost, opts.deviceTokURL, strings.NewReader(string(body)))
		req.Header.Set("content-type", "application/json")
		resp, err := opts.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("poll device token: %w", err)
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			var cs codeSuccessResponse
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
		code := devicePollErrorCode(rb)
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
			return nil, fmt.Errorf("device token poll: HTTP %d: %s", resp.StatusCode, truncate(string(rb), 200))
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
	return nil, fmt.Errorf("device login timed out after 15 minutes")
}

// exchangeCodeForTokens trades the authorization_code for access/refresh/id tokens.
func exchangeCodeForTokens(opts *codexLoginServerOptions, clientID, authCode, codeVerifier string) (*codexAuthFile, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {codexOAuthCallback},
		"client_id":     {clientID},
		"code_verifier": {codeVerifier},
	}
	req, _ := http.NewRequest(http.MethodPost, opts.tokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := opts.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange code: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("exchange code: HTTP %d: %s", resp.StatusCode, truncate(string(rb), 200))
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
		return nil, fmt.Errorf("token response missing access_token: %s", truncate(string(rb), 200))
	}
	af := &codexAuthFile{AuthMode: "chatgpt"}
	af.Tokens.AccessToken = tok.AccessToken
	af.Tokens.RefreshToken = tok.RefreshToken
	af.Tokens.IDToken = tok.IDToken
	af.Tokens.AccountID = accountIDFromTokens(tok.IDToken, "")
	af.LastRefresh = time.Now().UTC().Format(time.RFC3339Nano)
	return af, nil
}

// cmdCodexLogin runs the device flow and stores the resulting tokens.
func cmdCodexLogin(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	_ = cfg // config loaded but authFile is derived from provider name
	authFile := authFilePath("codex", "oauth_auth")
	opts := &codexLoginServerOptions{}
	opts.defaults()

	fmt.Println("Requesting device code from OpenAI...")
	uc, err := requestUserCode(opts, codexOAuthClientID)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\n  Open this URL: %s\n", codexOAuthVerifyURL)
	fmt.Printf("  Enter code:   %s\n\n", uc.UserCode)
	fmt.Println("Waiting for authorization (15 min timeout)...")

	interval, _ := strconv.Atoi(uc.Interval)
	cs, err := pollForToken(opts, uc.DeviceAuthID, uc.UserCode, interval)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Authorized. Exchanging code for tokens...")
	af, err := exchangeCodeForTokens(opts, codexOAuthClientID, cs.AuthorizationCode, cs.CodeVerifier)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(dirOf(authFile), 0o700); err != nil {
		log.Fatal(err)
	}
	b, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(authFile, b, 0o600); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("✓ codex OAuth tokens saved to %s\n", authFile)
	if af.Tokens.AccountID != "" {
		fmt.Printf("  account_id: %s\n", af.Tokens.AccountID)
	}
	fmt.Println("\nYou can now use codex-native models (gpt-5.5) through the proxy.")
}

// dirOf returns the directory of a path, or "." if none.
func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return "."
}
