// Package clicommon owns the small shared helpers used across CLI command
// packages (daemon HTTP fetch, section joining, pluralization) without pulling
// in the whole cli package.
package clicommon

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"model-proxy/internal/appapi"
	"model-proxy/internal/daemonctl"
	"model-proxy/internal/provider"
)

// StatusGet fetches base+path via the shared daemon client. A non-2xx status
// is NOT an error here — the caller inspects it.
func StatusGet(base, path string) (body []byte, status int, err error) {
	resp, err := daemonctl.Client.Get(base + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// AppendSection writes a non-empty section followed by one blank separator.
func AppendSection(b *strings.Builder, s string) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return
	}
	b.WriteString(s)
	b.WriteString("\n\n")
}

// RenderSchedule renders the schedule section of the status/doctor reports.
func RenderScheduleRoutes(models map[string]appapi.StatusRoute, ind string) string {
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, m := range names {
		ri := models[m]
		fmt.Fprintf(&b, "%s%s → %s\n", ind, provider.Bold(m), provider.Green(ri.First))
		if ri.Pin != "" {
			exp := ""
			if ri.PinExpires != "" {
				exp = provider.Dim(" (" + ri.PinExpires + ")")
			}
			fmt.Fprintf(&b, "%s    %s%s%s\n", ind, provider.Yellow("pinned: "), ri.Pin, exp)
		}
		for _, pool := range ri.Pools {
			fmt.Fprintf(&b, "%s    %s %s (%d accounts, %d available)\n",
				ind, provider.Dim("pool:"), provider.Bold(pool.Parent), pool.Accounts, pool.Available)
		}
		for _, t := range ri.Ordered {
			extra := ""
			if !t.Available {
				extra += " " + provider.Red("(unavailable)")
			}
			if t.Peak {
				extra += " " + provider.Yellow("peak")
			}
			fmt.Fprintf(&b, "%s    %s %s  surplus %+.2f  p%d%s\n",
				ind, provider.Pad(t.Provider, 14), provider.Gray(provider.Pad(t.Tier, 13)), t.Surplus, t.Priority, extra)
		}
		if ri.Sticky != "" {
			dwell := ""
			if ri.DwellRem > 0 {
				dwell = fmt.Sprintf(", %.0fs dwell left", ri.DwellRem)
			}
			fmt.Fprintf(&b, "%s    %s%s%s\n", ind, provider.Dim("sticky: "), ri.Sticky, provider.Dim(dwell))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderSchedule renders the serve-status Schedule section: header + the shared
// per-route renderer at 2-space indent. (Trailing-newline normalization is
// handled once by appendSection.)
func RenderSchedule(st *appapi.StatusResp) string {
	if len(st.Schedule.Models) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d %s)\n", provider.Bold("Schedule"), len(st.Schedule.Models), plural(len(st.Schedule.Models), "route", "routes"))
	b.WriteString(RenderScheduleRoutes(st.Schedule.Models, "  "))
	return b.String()
}

func plural(n int, sing, plur string) string {
	if n == 1 {
		return sing
	}
	return plur
}
