package admin

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/login"
	"model-proxy/internal/provider"
)

func aqpMock(t *testing.T, provisionStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/compass-api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"result":"https://soup.shopee.io/login"}`)
	})
	mux.HandleFunc("/compass-api/v1/auth/info", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: provider.SsoCookieName, Value: "test-sso-c", Path: "/"})
		fmt.Fprint(w, `{"retcode":0,"data":{"user":{"userid":1,"email":"u@x.com","is_active":true}}}`)
	})
	mux.HandleFunc("/api/v1/cqp/ccswitch/api_key/get_or_generate", func(w http.ResponseWriter, r *http.Request) {
		if provisionStatus != http.StatusOK {
			http.Error(w, "provisioning down", provisionStatus)
			return
		}
		fmt.Fprint(w, `{"retcode":0,"data":{"api_key":"managed-key","project_id":"proj","employee_email":"u@x.com"}}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func aqpService(up *httptest.Server, spy *reloadSpy) *Service {
	return New(Ports{
		ProviderConfig: func(string) (configdomain.Provider, bool) {
			return configdomain.Provider{Provider: "aqp"}, true
		},
		ConfigFile: func() string { return "config.yaml" },
		Reload:     spy.fn(),
		NewAqpClient: func(store string) *login.AqpClient {
			client := login.NewAqpClient(store)
			client.Base = up.URL
			return client
		},
	})
}

func TestBeginLoginUnknownAndNonAsync(t *testing.T) {
	service := New(Ports{
		ProviderConfig: func(name string) (configdomain.Provider, bool) {
			if name == "zhipu" {
				return configdomain.Provider{Provider: "zhipu"}, true
			}
			return configdomain.Provider{}, false
		},
	})
	if _, err := service.BeginLogin(context.Background(), "nope"); httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("unknown provider status = %d", httpErrorStatus(t, err))
	}
	_, err := service.BeginLogin(context.Background(), "zhipu")
	if httpErrorStatus(t, err) != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "has no async login flow") {
		t.Errorf("non-async err = %v", err)
	}
}

func TestAqpLoginFlow(t *testing.T) {
	setTestHome(t)
	spy := &reloadSpy{}
	service := aqpService(aqpMock(t, http.StatusOK), spy)

	start, err := service.BeginLogin(context.Background(), "aqp")
	if err != nil {
		t.Fatal(err)
	}
	if start.Provider != "aqp" || start.LoginURL != "https://soup.shopee.io/login" {
		t.Fatalf("start = %+v", start)
	}
	update := start.Job.Run(context.Background())
	if update.State != "done" || update.Result != "u@x.com" || update.Warning != "" {
		t.Fatalf("update = %+v", update)
	}
	if update.Detail != "https://soup.shopee.io/login" {
		t.Errorf("detail = %q", update.Detail)
	}
	account, err := provider.LoadAqpAccount(accounts.AuthFilePath("aqp", "oauth_auth"))
	if err != nil || account == nil {
		t.Fatalf("persisted aqp account = %+v, %v", account, err)
	}
	if account.Email != "u@x.com" || account.ProjectID != "proj" || account.SSOSessionCookie == "" {
		t.Errorf("account = %+v", account)
	}
	if len(spy.calls) != 1 {
		t.Errorf("reload calls = %v", spy.calls)
	}
}

func TestAqpLoginReloadWarningSurfacesInUpdate(t *testing.T) {
	setTestHome(t)
	spy := &reloadSpy{err: &ReloadAppliedWarning{Err: errors.New("state degraded")}}
	service := aqpService(aqpMock(t, http.StatusOK), spy)

	start, err := service.BeginLogin(context.Background(), "aqp")
	if err != nil {
		t.Fatal(err)
	}
	update := start.Job.Run(context.Background())
	if update.State != "done" ||
		update.Warning != "reload applied with warning: state degraded" {
		t.Fatalf("update = %+v", update)
	}
}

func TestAqpLoginBootstrapFailure(t *testing.T) {
	setTestHome(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"oops":"no url here"}`)
	}))
	defer up.Close()
	service := aqpService(up, &reloadSpy{})

	_, err := service.BeginLogin(context.Background(), "aqp")
	if httpErrorStatus(t, err) != http.StatusBadGateway {
		t.Errorf("bootstrap failure status = %d, want 502", httpErrorStatus(t, err))
	}
}

func TestAqpLoginJobProvisioningError(t *testing.T) {
	setTestHome(t)
	service := aqpService(aqpMock(t, http.StatusInternalServerError), &reloadSpy{})

	start, err := service.BeginLogin(context.Background(), "aqp")
	if err != nil {
		t.Fatal(err)
	}
	update := start.Job.Run(context.Background())
	if update.State != "error" || !strings.Contains(update.Result, "api key provisioning") {
		t.Fatalf("update = %+v", update)
	}
}

func codexMock(t *testing.T, tokenStatus int) *httptest.Server {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1"}}`))
	fakeIDToken := "h." + payload + ".s"
	mux := http.NewServeMux()
	mux.HandleFunc("/usercode", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"device_auth_id":"daid","user_code":"CODE","interval":"1"}`)
	})
	mux.HandleFunc("/devtok", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_code":"ac","code_challenge":"cc","code_verifier":"cv"}`)
	})
	mux.HandleFunc("/tok", func(w http.ResponseWriter, r *http.Request) {
		if tokenStatus != http.StatusOK {
			http.Error(w, "exchange down", tokenStatus)
			return
		}
		fmt.Fprint(w, `{"access_token":"at","refresh_token":"rt","id_token":"`+fakeIDToken+`"}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func codexService(up *httptest.Server, spy *reloadSpy) *Service {
	return New(Ports{
		ProviderConfig: func(string) (configdomain.Provider, bool) {
			return configdomain.Provider{Provider: "codex"}, true
		},
		ConfigFile: func() string { return "config.yaml" },
		Reload:     spy.fn(),
		NewCodexOptions: func() *login.CodexLoginServerOptions {
			options := &login.CodexLoginServerOptions{}
			options.Defaults()
			options.UsercodeURL = up.URL + "/usercode"
			options.DeviceTokURL = up.URL + "/devtok"
			options.TokenURL = up.URL + "/tok"
			return options
		},
	})
}

func TestCodexLoginFlow(t *testing.T) {
	setTestHome(t)
	spy := &reloadSpy{}
	service := codexService(codexMock(t, http.StatusOK), spy)

	start, err := service.BeginLogin(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if start.Provider != "codex" || start.UserCode != "CODE" ||
		start.VerifyURL != login.CodexOAuthVerifyURL {
		t.Fatalf("start = %+v", start)
	}
	update := start.Job.Run(context.Background())
	if update.State != "done" || update.Result != "acct-1" || update.Warning != "" {
		t.Fatalf("update = %+v", update)
	}
	info, err := os.Stat(accounts.AuthFilePath("codex", "oauth_auth"))
	if err != nil {
		t.Fatalf("codex auth file not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("codex auth file perm = %o, want 0600", perm)
	}
	if len(spy.calls) != 1 {
		t.Errorf("reload calls = %v", spy.calls)
	}
}

func TestCodexLoginUserCodeFailure(t *testing.T) {
	setTestHome(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead.Close()
	service := New(Ports{
		ProviderConfig: func(string) (configdomain.Provider, bool) {
			return configdomain.Provider{Provider: "codex"}, true
		},
		NewCodexOptions: func() *login.CodexLoginServerOptions {
			options := &login.CodexLoginServerOptions{UsercodeURL: dead.URL + "/usercode"}
			options.Defaults()
			return options
		},
	})
	_, err := service.BeginLogin(context.Background(), "codex")
	if httpErrorStatus(t, err) != http.StatusBadGateway {
		t.Errorf("user-code failure status = %d, want 502", httpErrorStatus(t, err))
	}
}

func TestCodexLoginJobExchangeError(t *testing.T) {
	setTestHome(t)
	service := codexService(codexMock(t, http.StatusInternalServerError), &reloadSpy{})

	start, err := service.BeginLogin(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	update := start.Job.Run(context.Background())
	if update.State != "error" || update.Result == "" {
		t.Fatalf("update = %+v", update)
	}
	if !strings.Contains(update.Detail, "code: CODE") {
		t.Errorf("detail = %q", update.Detail)
	}
}
