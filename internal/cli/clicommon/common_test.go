package clicommon_test

import (
	"errors"
	"io"
	"model-proxy/internal/display"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
	"model-proxy/internal/cli/clicommon"
	"model-proxy/internal/daemonctl"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestStatusGetReturnsExactNonSuccessResponse(t *testing.T) {
	original := daemonctl.Client
	t.Cleanup(func() { daemonctl.Client = original })

	bodyClosed := false
	daemonctl.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("method = %q, want GET", req.Method)
		}
		if got, want := req.URL.String(), "http://daemon.test/api/status?detail=1"; got != want {
			t.Fatalf("URL = %q, want %q", got, want)
		}
		return &http.Response{
			StatusCode: http.StatusTeapot,
			Body: &trackingReadCloser{
				Reader: strings.NewReader("daemon says no"),
				closed: &bodyClosed,
			},
			Header: make(http.Header),
		}, nil
	})}

	body, status, err := clicommon.StatusGet("http://daemon.test", "/api/status?detail=1")
	if err != nil {
		t.Fatalf("StatusGet: %v", err)
	}
	if status != http.StatusTeapot {
		t.Fatalf("status = %d, want %d", status, http.StatusTeapot)
	}
	if got, want := string(body), "daemon says no"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if !bodyClosed {
		t.Fatal("response body was not closed")
	}
}

func TestStatusGetReturnsTransportError(t *testing.T) {
	original := daemonctl.Client
	t.Cleanup(func() { daemonctl.Client = original })

	wantErr := errors.New("transport unavailable")
	daemonctl.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, wantErr
	})}

	body, status, err := clicommon.StatusGet("http://daemon.test", "/api/status")
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if body != nil {
		t.Fatalf("body = %q, want nil", body)
	}
	if status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
}

func TestAppendSectionExactSeparators(t *testing.T) {
	var b strings.Builder
	clicommon.AppendSection(&b, "")
	clicommon.AppendSection(&b, "\n\n")
	clicommon.AppendSection(&b, "alpha\n\n")
	clicommon.AppendSection(&b, "beta")

	if got, want := b.String(), "alpha\n\nbeta\n\n"; got != want {
		t.Fatalf("AppendSection output = %q, want %q", got, want)
	}
}

func TestRenderScheduleExactOutput(t *testing.T) {
	originalColor := display.ColorEnabled
	display.SetColorEnabled(false)
	t.Cleanup(func() { display.SetColorEnabled(originalColor) })

	status := &appapi.StatusResp{Schedule: appapi.StatusSchedule{Models: map[string]appapi.StatusRoute{
		"beta": {
			First:  "first-b",
			Pin:    "pin-b",
			Sticky: "sticky-b",
		},
		"alpha": {
			First:      "first-a",
			Pin:        "pin-a",
			PinExpires: "soon",
			Pools: []appapi.StatusPool{
				{Parent: "pool-a", Accounts: 3, Available: 2},
			},
			Ordered: []appapi.StatusOrdered{
				{Provider: "down", Tier: "burst", Surplus: -1.25, Priority: 7, Available: false, Peak: true},
				{Provider: "up", Tier: "steady", Surplus: 2.5, Priority: 1, Available: true},
			},
			Sticky:   "sticky-a",
			DwellRem: 12.4,
		},
	}}}

	got := strings.Split(clicommon.RenderSchedule(status), "\n")
	want := []string{
		"Schedule (2 routes)",
		"  alpha → first-a",
		"      pinned: pin-a (soon)",
		"      pool: pool-a (3 accounts, 2 available)",
		"      down" + strings.Repeat(" ", 11) + "burst" + strings.Repeat(" ", 10) + "surplus -1.25  p7 (unavailable) peak",
		"      up" + strings.Repeat(" ", 13) + "steady" + strings.Repeat(" ", 9) + "surplus +2.50  p1",
		"      sticky: sticky-a, 12s dwell left",
		"",
		"  beta → first-b",
		"      pinned: pin-b",
		"      sticky: sticky-b",
		"",
		"",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RenderSchedule lines:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestRenderScheduleEmptyAndSingular(t *testing.T) {
	originalColor := display.ColorEnabled
	display.SetColorEnabled(false)
	t.Cleanup(func() { display.SetColorEnabled(originalColor) })

	if got := clicommon.RenderSchedule(&appapi.StatusResp{}); got != "" {
		t.Fatalf("empty schedule = %q, want empty", got)
	}
	status := &appapi.StatusResp{Schedule: appapi.StatusSchedule{Models: map[string]appapi.StatusRoute{
		"solo": {First: "only"},
	}}}
	if got, want := clicommon.RenderSchedule(status), "Schedule (1 route)\n  solo → only\n\n"; got != want {
		t.Fatalf("singular schedule = %q, want %q", got, want)
	}
}

type trackingReadCloser struct {
	io.Reader
	closed *bool
}

func (r *trackingReadCloser) Close() error {
	*r.closed = true
	return nil
}
