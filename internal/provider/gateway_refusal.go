package provider

import (
	"net/http"
	"regexp"
	"strings"
)

// A gateway refusal is an edge / WAF answer, not an API reply: the nginx
// family's stock 403 document (nginx / openresty / tengine ship the same
// page with only the footer banner different) or a Cloudflare challenge. It
// says nothing about the credential — the same key is served from another
// network — so no surface may read it as a token problem. One refusal is
// cheap to absorb (a WAF rule can be request-specific); a run of them means
// the gateway is refusing this client, and the account's circuit takes over
// (runtime.RecordGatewayRefusal).
//
// The vocabulary lives here because three paths meet the same page in three
// shapes: the forward path sees headers plus the whole body (GatewayRefused),
// the auth layer (a key mint) and the quota poll only keep a bounded body
// excerpt inside an error string (GatewayRefusalText).

// nginxForbidden matches the WHOLE stock denial document, never a server
// banner on its own. Content-Type alone says nothing about the refusal.
var nginxForbidden = regexp.MustCompile(`(?i)^\s*<html>\s*<head><title>403 Forbidden</title></head>\s*<body>\s*<center><h1>403 Forbidden</h1></center>\s*<hr><center>(?:nginx|openresty|tengine)(?:/[0-9.]+)?</center>\s*</body>\s*</html>\s*$`)

// GatewayRefused reports a gateway refusal from the complete evidence of a
// response: a Cloudflare challenge (decided by its header, whatever the body)
// or the nginx family's stock 403 document.
func GatewayRefused(header http.Header, body []byte) bool {
	return strings.EqualFold(header.Get("Cf-Mitigated"), "challenge") || nginxForbidden.Match(body)
}

// gatewayBanner matches the family's server banner on its stock page: the
// name, an optional version, then the closing tag. The evidence an error
// string carries is bounded and gets cut anywhere — the live shape ends in
// `<center>nginx</cente...` — so the banner is recognised by what follows
// the name (a `<`), never by a complete closing tag; a name that merely
// contains a family word (nginx-plus, mynginx) has no `<` right after it.
var gatewayBanner = regexp.MustCompile(`(?i)<center>(?:nginx|openresty|tengine)(?:/[0-9.]+)?<`)

// GatewayRefusalText reports whether a bounded evidence string names the
// nginx family's refusal document: the 403 status, the page's title and the
// family's server banner, all three. The title alone is not evidence —
// another server's 403 page and an HTML error page of the provider's own can
// carry the same title.
func GatewayRefusalText(text string) bool {
	return strings.Contains(strings.ToLower(text), "http 403") &&
		strings.Contains(strings.ToLower(text), "<title>403 forbidden</title>") &&
		gatewayBanner.MatchString(text)
}
