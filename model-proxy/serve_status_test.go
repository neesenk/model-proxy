package main

import (
	"testing"
	"time"
)

func TestCompactNum(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{5, "5"},
		{567, "567"},
		{1000, "1k"},
		{1234, "1.2k"},
		{450000, "450k"},
		{1000000, "1M"},
		{1200000, "1.2M"},
		{1000000000, "1B"},
		{5600000000, "5.6B"},
	}
	for _, c := range cases {
		if got := compactNum(c.in); got != c.want {
			t.Errorf("compactNum(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatClock(t *testing.T) {
	if got := formatClock(0); got != "—" {
		t.Errorf("formatClock(0) = %q, want —", got)
	}
	if got := formatClock(-5); got != "—" {
		t.Errorf("formatClock(-5) = %q, want —", got)
	}
	want := time.Unix(1700000000, 0).Local().Format("15:04:05")
	if got := formatClock(1700000000); got != want {
		t.Errorf("formatClock(1700000000) = %q, want %q", got, want)
	}
}

func TestFormatClockTime(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, now.Location())
	if got := formatClockTime(today); got != "09:30" {
		t.Errorf("today = %q, want 09:30", got)
	}
	other := time.Date(2024, 1, 2, 9, 30, 0, 0, now.Location())
	if got := formatClockTime(other); got != "01-02 09:30" {
		t.Errorf("other-day = %q, want 01-02 09:30", got)
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "route", "routes"); got != "route" {
		t.Errorf("plural(1) = %q, want route", got)
	}
	if got := plural(3, "route", "routes"); got != "routes" {
		t.Errorf("plural(3) = %q, want routes", got)
	}
}
