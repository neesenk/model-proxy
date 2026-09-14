package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// usage_fetch_test.go pins the shared quota-GET contract for every billing
// provider: auth failure / transport failure / non-200 all become
// BillingUnknown snapshots (never non-nil errors), status hints are honored,
// and success returns the raw body.

func okAuth(*http.Request) error { return nil }

func failAuth(*http.Request) error { return errors.New("no credentials") }

func TestUsageGetAuthFailureBecomesBillingUnknown(t *testing.T) {
	body, fail, ok := usageGet("http://127.0.0.1/unreachable", failAuth, nil, nil)
	if ok || body != nil {
		t.Fatal("auth failure must short-circuit the request")
	}
	if fail == nil || fail.Billing != BillingUnknown || fail.Err != "no credentials" {
		t.Errorf("fail = %+v, want BillingUnknown with the auth error", fail)
	}
}

func TestUsageGetTransportFailureBecomesBillingUnknown(t *testing.T) {
	// A server that is closed before the request forces a transport error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	_, fail, ok := usageGet(srv.URL, okAuth, nil, nil)
	if ok {
		t.Fatal("transport failure must not be ok")
	}
	if fail == nil || fail.Billing != BillingUnknown || fail.Err == "" {
		t.Errorf("fail = %+v, want BillingUnknown with the transport error", fail)
	}
}

func TestUsageGetNon200UsesDefaultAndCustomHints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()

	_, fail, ok := usageGet(srv.URL, okAuth, nil, nil)
	if ok || fail.Err != "HTTP 418" {
		t.Errorf("default hint: fail.Err = %q, want %q", fail.Err, "HTTP 418")
	}

	_, fail, ok = usageGet(srv.URL, okAuth, nil, func(code int) string {
		return "custom " + http.StatusText(code)
	})
	if ok || fail.Err != "custom I'm a teapot" {
		t.Errorf("custom hint: fail.Err = %q, want %q", fail.Err, "custom I'm a teapot")
	}
}

func TestUsageGetSuccessPassesBodyAndHeaders(t *testing.T) {
	var gotAuth, gotExtra string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotExtra = r.Header.Get("X-Extra")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	auth := func(r *http.Request) error { r.Header.Set("Authorization", "Bearer k"); return nil }
	body, fail, ok := usageGet(srv.URL, auth, map[string]string{"X-Extra": "v"}, nil)
	if !ok || fail != nil {
		t.Fatalf("ok = %v, fail = %+v; want success", ok, fail)
	}
	if string(body) != `{"ok":true}` {
		t.Errorf("body = %q", body)
	}
	if gotAuth != "Bearer k" || gotExtra != "v" {
		t.Errorf("headers = %q/%q, want auth + extra both set", gotAuth, gotExtra)
	}
}

func TestBigmodelQuotaMatrix(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantErr    string
		wantParsed bool
	}{
		{"auth-adjacent 401", http.StatusUnauthorized, "denied", "HTTP 401", false},
		{"server error", http.StatusInternalServerError, "boom", "HTTP 500", false},
		{"success wrong shape", http.StatusOK, `{"hello":"world"}`, "not zhipu quota format", false},
		{"success zhipu envelope", http.StatusOK, `{"success":true,"data":{"level":"pro","limits":[{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000}]}}`, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			snap, err := bigmodelQuota(srv.URL, okAuth, nil)
			if err != nil {
				t.Fatalf("bigmodelQuota returned a non-nil error: %v", err)
			}
			if snap == nil {
				t.Fatal("snapshot must never be nil")
			}
			if tc.wantParsed {
				if snap.Err != "" || snap.Level != "pro" {
					t.Errorf("snap = %+v, want parsed envelope", snap)
				}
				return
			}
			if snap.Billing != BillingUnknown || snap.Err != tc.wantErr {
				t.Errorf("snap = %+v, want BillingUnknown Err=%q", snap, tc.wantErr)
			}
		})
	}
}
