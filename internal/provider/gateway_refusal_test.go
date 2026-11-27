package provider

import (
	"net/http"
	"testing"
)

func denialPage(banner string) string {
	return "<html>\n<head><title>403 Forbidden</title></head>\n<body>\n<center><h1>403 Forbidden</h1></center>\n<hr><center>" + banner + "</center>\n</body>\n</html>"
}

// TestGatewayRefusedNginxFamilyDenialPage: the gateway-refusal evidence is
// the WHOLE stock denial document, never a server banner on its own. nginx,
// openresty and tengine are one family — the forks ship the same default 403
// page with only the footer banner different — so every family banner matches
// with or without a version. Anything else stays an ordinary 403: another
// server's banner on the same page shape, a banner that merely contains a
// family name, or a family banner on a page that is not the whole document.
func TestGatewayRefusedNginxFamilyDenialPage(t *testing.T) {
	for _, banner := range []string{
		"nginx",
		"nginx/1.24.0",
		"openresty",
		"openresty/1.21.4.1",
		"OpenResty/1.27.1.2",
		"tengine",
		"tengine/2.3.3",
	} {
		if !GatewayRefused(http.Header{}, []byte(denialPage(banner))) {
			t.Errorf("stock denial page with banner %q = false, want a gateway refusal", banner)
		}
	}
	for _, banner := range []string{
		"Apache/2.4.62 (Unix)",
		"Microsoft-IIS/10.0",
		"nginx-plus",
		"mynginx",
		"openresty-admin",
	} {
		if GatewayRefused(http.Header{}, []byte(denialPage(banner))) {
			t.Errorf("denial page with banner %q = true, want an ordinary 403", banner)
		}
	}
	// A family banner alone is not evidence: the page must be the whole
	// document, and Cf-Mitigated still decides independently of the body.
	if GatewayRefused(http.Header{}, []byte("<html><head><title>403 Forbidden</title></head><body><hr><center>openresty</center></body></html>")) {
		t.Error("openresty banner on a page without the <h1> line = true, want an ordinary 403")
	}
	if GatewayRefused(http.Header{}, []byte(denialPage("tengine")+"\n<script src='/waf.js'></script>")) {
		t.Error("denial page with trailing markup = true, want an ordinary 403")
	}
	if !GatewayRefused(http.Header{"Cf-Mitigated": []string{"challenge"}}, nil) {
		t.Error("Cf-Mitigated: challenge = false, want a gateway refusal")
	}
}

// GatewayRefusalText reads the bounded excerpt an error string carries — the
// auth layer's mint failure and the quota poll's sync failure. The live shape
// (serve.log, 2026-11-26): the stock page flattened onto one line and cut
// inside the server banner.
func TestGatewayRefusalText(t *testing.T) {
	live := `mint aqp key: HTTP 403: <html> <head><title>403 Forbidden</title></head> <body> <center><h1>403 Forbidden</h1></center> <hr><center>nginx</cente...`
	if !GatewayRefusalText(live) {
		t.Error("the live flattened, truncated denial page = false, want a gateway refusal")
	}
	for _, banner := range []string{"openresty/1.21.4.1", "TEngine"} {
		if !GatewayRefusalText("monthly usage HTTP 403: " + denialPage(banner)) {
			t.Errorf("denial page with banner %q = false, want a gateway refusal", banner)
		}
	}
	// The title alone is not evidence, and neither is the banner alone.
	for text, why := range map[string]string{
		"HTTP 403: <html><head><title>403 Forbidden</title></head><body>blocked by WAF</body></html>": "a 403 page without the family banner",
		"HTTP 403: <hr><center>nginx</center>":                                                        "the family banner without the title",
		`{"error":{"message":"model forbidden for this key","type":"permission_error"}}`:              "the provider's own JSON 403",
		"HTTP 401: <title>403 Forbidden</title><center>nginx</center>":                                "not a 403",
		"HTTP 403: <title>403 Forbidden</title><center>nginx-plus</center>":                           "a banner that merely contains a family name",
		"HTTP 403: <title>403 Forbidden</title><center>mynginx</center>":                              "a banner that merely contains a family name",
	} {
		if GatewayRefusalText(text) {
			t.Errorf("%s = true, want an ordinary error:\n%s", why, text)
		}
	}
}
