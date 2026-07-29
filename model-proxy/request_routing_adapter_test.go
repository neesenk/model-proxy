package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestForceProvider(t *testing.T) {
	request := &http.Request{
		Header: http.Header{"X-Mp-Force-Provider": []string{"header-provider"}},
		URL: &url.URL{
			RawQuery: "force_provider=query-provider",
		},
	}
	if got := forcedProviderFromRequest(request); got != "header-provider" {
		t.Errorf("header precedence = %q, want header-provider", got)
	}
	request.Header.Del("x-mp-force-provider")
	if got := forcedProviderFromRequest(request); got != "query-provider" {
		t.Errorf("query fallback = %q, want query-provider", got)
	}
	if got := forcedProviderFromRequest(nil); got != "" {
		t.Errorf("nil request = %q, want empty", got)
	}
	if got := forcedProviderFromRequest(&http.Request{}); got != "" {
		t.Errorf("nil URL = %q, want empty", got)
	}
}
