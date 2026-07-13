package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Volcengine OpenAPI (control-plane) request signing (signature V4 / HMAC-SHA256).
// Used for GetAFPUsage (Agent Plan AFP quota) - the Ark API Key (Bearer) can't
// reach these signed APIs; they need the Volcengine AccessKey/SecretKey.
//
// See https://www.volcengine.com/docs/6369/67268 (公共参数) + 67270 (签名过程).
// Credential scope: {YYYYMMDD}/{Region}/{Service}/request
// Signing key: HMAC chain SK -> kDate -> kRegion -> kService -> kSigning (final
// term "request", NOT "volcengine_request" - per the official signing demo).

// volcengineSignV4 computes the X-Date and Authorization header values for a
// Volcengine OpenAPI request. canonicalQuery is the sorted, RFC3986-escaped query
// string (must match the URL's query exactly). body is the request body (empty for GET).
func volcengineSignV4(method, host, path, canonicalQuery string, body []byte, now time.Time, ak, sk, region, service string) (xDate, authorization string) {
	xDate = now.UTC().Format("20060102T150405Z")
	shortDate := now.UTC().Format("20060102")
	payloadHash := sha256Hex(body)

	// Signed headers: host + x-date only. (The payload hash is still the 6th
	// canonical-request component, but Volcengine does NOT sign x-content-sha256
	// as a header - per the official signing demo. Sending/signing it causes
	// "Invalid Authorization".)
	headers := [][2]string{
		{"host", host},
		{"x-date", xDate},
	}
	canonicalHeaders, signedHeaders := canonicalHeaders(headers)
	if path == "" {
		path = "/"
	}
	canonicalRequest := strings.Join([]string{
		method,
		path,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := shortDate + "/" + region + "/" + service + "/request"
	stringToSign := strings.Join([]string{
		"HMAC-SHA256",
		xDate,
		credentialScope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kSigning := deriveSigningKey(sk, shortDate, region, service)
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
	authorization = "HMAC-SHA256 Credential=" + ak + "/" + credentialScope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + signature
	return
}

// volcengineGet builds a signed GET request to the Volcengine OpenAPI
// (open.volcengineapi.com) for the given Action/Version. extraQuery is optional
// additional query params (key=value pairs, already escaped) appended after Version.
func volcengineGet(action, version, ak, sk string, now time.Time, extraQuery string) (*http.Request, error) {
	host := "open.volcengineapi.com"
	canonicalQuery := "Action=" + volcEscape(action) + "&Version=" + volcEscape(version)
	if extraQuery != "" {
		canonicalQuery += "&" + extraQuery
	}
	body := []byte("") // GET, no body
	xDate, authz := volcengineSignV4("GET", host, "/", canonicalQuery, body, now, ak, sk, "cn-beijing", "ark")
	req, err := http.NewRequest("GET", "https://"+host+"/?"+canonicalQuery, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Host", host)
	req.Header.Set("X-Date", xDate)
	req.Header.Set("Authorization", authz)
	return req, nil
}

// VolcengineSignedGet builds a signed GET request to the Volcengine OpenAPI for
// the given Action/Version (AK/SK auth). Exported so the main package's model-list
// path (ListArkAgentPlanModel, wired as FetchModelsFn until Phase 5) can reuse the
// provider's signing without re-implementing it.
func VolcengineSignedGet(action, version, ak, sk string, now time.Time, extraQuery string) (*http.Request, error) {
	return volcengineGet(action, version, ak, sk, now, extraQuery)
}

func deriveSigningKey(sk, shortDate, region, service string) []byte {
	kDate := hmacSHA256([]byte(sk), shortDate)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "request")
}

// canonicalHeaders sorts headers by lowercased name, builds the canonical header
// block ("name:value\n"...) and the signed-headers list ("name;name...").
func canonicalHeaders(headers [][2]string) (canonical, signed string) {
	sort.Slice(headers, func(i, j int) bool { return headers[i][0] < headers[j][0] })
	var c strings.Builder
	names := make([]string, 0, len(headers))
	for _, h := range headers {
		c.WriteString(h[0] + ":" + strings.TrimSpace(h[1]) + "\n")
		names = append(names, h[0])
	}
	return c.String(), strings.Join(names, ";")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// volcEscape applies RFC 3986 percent-encoding (space -> %20), matching Volcengine's
// canonical-query-string encoding.
func volcEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
