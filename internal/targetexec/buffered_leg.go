package targetexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// BufferedLeg runs one non-streaming upstream round-trip against a planned
// target: URL build + provider rewrite + param-block application, POST with
// a JSON body, then the standard one-shot retry policies (401 auth refresh,
// 400 unsupported-parameter learn/strip). It is the headless counterpart of
// Executor for callers that need bytes instead of a client stream (fusion
// panel/judge legs). Effect recording (circuit/metrics/rate-limit) stays with
// the caller.
type BufferedLeg struct {
	Client Doer // required
	Plan   Plan // target plan (provider impl, base URL, path, headers)

	// MaxBody caps the response read; <= 0 selects defaultBufferedLegMaxBody.
	MaxBody         int64
	ApplyParamBlock func(body []byte) []byte // nil = identity
	LearnParamBlock func(param string)       // nil = no-op
	OnStripParam    func(param string)       // optional log hook

	// Capture, when non-nil, receives the final exchange (last built request,
	// last upstream response, exact body bytes sent) so the caller can feed
	// its request log without re-deriving provider-rewritten wire facts.
	Capture *BufferedLegExchange
}

// BufferedLegExchange is the final wire exchange of one BufferedLeg run. The
// response Body has already been drained and closed; consumers may only read
// StatusCode and Header.
type BufferedLegExchange struct {
	Request  *http.Request
	Response *http.Response
	SentBody []byte
}

// defaultBufferedLegMaxBody caps the buffered response read when the caller
// does not set MaxBody (fusion legs pass 64<<20 explicitly).
const defaultBufferedLegMaxBody = 64 << 20

// BufferedLegBuildError wraps a Do failure that happened before any byte
// reached the wire (request build or auth header construction), distinguishing
// it from transport/response-read failures: callers record provider failures
// only for the latter.
type BufferedLegBuildError struct{ Err error }

func (e *BufferedLegBuildError) Error() string { return e.Err.Error() }
func (e *BufferedLegBuildError) Unwrap() error { return e.Err }

// Do executes the round-trip. A transport/read error is returned as err — the
// caller classifies caller-side cancellation (ctx.Err()) vs provider failure.
// status is the last received upstream status (0 when no upstream response
// was ever received), so a retry that fails on the wire still reports the
// status that triggered the retry. Request-build/auth failures are returned
// as *BufferedLegBuildError (auth failures wrapped as "auth: %w" inside).
func (leg BufferedLeg) Do(ctx context.Context, body []byte) (status int, respBody []byte, err error) {
	if leg.Client == nil {
		return 0, nil, errors.New("targetexec.BufferedLeg: nil Client")
	}
	impl := leg.Plan.Provider()
	if impl == nil {
		return 0, nil, fmt.Errorf("targetexec.BufferedLeg: no runtime provider implementation for %s", leg.Plan.Target().Provider)
	}
	maxBody := leg.MaxBody
	if maxBody <= 0 {
		maxBody = defaultBufferedLegMaxBody
	}
	var (
		req           *http.Request
		resp          *http.Response
		refreshedAuth bool
		strippedParam bool
	)
	// Same one-shot retry policies as the normal target pipeline: apply learned
	// param blocks before every send, refresh auth once on 401, learn/strip one
	// newly reported top-level parameter on 400. URL build, provider rewrite
	// and param-block application re-run every iteration against the CURRENT
	// body (a strip retry reshapes what the next send rewrites).
	for {
		targetURL := strings.TrimRight(leg.Plan.BaseURL(), "/") + leg.Plan.UpstreamPath()
		targetURL, body = impl.RewriteRequest(targetURL, body, leg.Plan.UpstreamPath())
		if leg.ApplyParamBlock != nil {
			body = leg.ApplyParamBlock(body)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
		if err != nil {
			return status, nil, &BufferedLegBuildError{Err: err}
		}
		req.Header.Set("content-type", "application/json")
		if err := impl.AuthHeaders(req); err != nil {
			return status, nil, &BufferedLegBuildError{Err: fmt.Errorf("auth: %w", err)}
		}
		leg.Plan.ApplyConfiguredHeaders(req.Header)
		impl.ExtraHeaders(req, leg.Plan.UpstreamPath())

		resp, err = leg.Client.Do(req)
		if err != nil {
			return status, nil, err
		}
		status = resp.StatusCode
		respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
		resp.Body.Close()
		if err != nil {
			return status, nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !refreshedAuth {
			refreshedAuth = true
			if refreshErr := impl.Refresh(); refreshErr == nil {
				continue
			}
		}
		if resp.StatusCode == http.StatusBadRequest && !strippedParam {
			if param, found := ParseUnsupportedParam(respBody); found {
				if leg.LearnParamBlock != nil {
					leg.LearnParamBlock(param)
				}
				if stripped, changed := StripTopLevelParam(body, param); changed {
					body = stripped
					strippedParam = true
					if leg.OnStripParam != nil {
						leg.OnStripParam(param)
					}
					continue
				}
			}
		}
		break
	}
	if leg.Capture != nil {
		*leg.Capture = BufferedLegExchange{Request: req, Response: resp, SentBody: body}
	}
	return status, respBody, nil
}
