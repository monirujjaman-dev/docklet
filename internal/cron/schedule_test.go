package cron

import (
	"testing"
	"time"
)

func TestScheduleNext(t *testing.T) {
	base := time.Date(2026, 11, 21, 10, 7, 30, 0, time.UTC) // Saturday
	for _, c := range []struct {
		expr, tz, want string
	}{
		{"* * * * *", "UTC", "2026-11-21T10:08:00Z"},
		{"*/15 * * * *", "UTC", "2026-11-21T10:15:00Z"},
		{"0 * * * *", "UTC", "2026-11-21T11:00:00Z"},
		{"30 2 * * *", "UTC", "2026-11-22T02:30:00Z"},
		{"0 9 * * mon-fri", "UTC", "2026-11-23T09:00:00Z"},
		{"0 0 1 * *", "UTC", "2026-12-01T00:00:00Z"},
		{"0 0 29 2 *", "UTC", "2028-02-29T00:00:00Z"},
		{"5,10 8-9/1 * * *", "UTC", "2026-11-22T08:05:00Z"},
		{"0 0 * * 7", "UTC", "2026-11-22T00:00:00Z"},
		{"@daily", "UTC", "2026-11-22T00:00:00Z"},
		{"@hourly", "UTC", "2026-11-21T11:00:00Z"},
		// 1st of the month OR Monday (Vixie semantics).
		{"0 0 1 * 1", "UTC", "2026-11-23T00:00:00Z"},
		// Asia/Dhaka is UTC+6: 03:00 local = 21:00 UTC the day before.
		{"0 3 * * *", "Asia/Dhaka", "2026-11-21T21:00:00Z"},
		// India is UTC+5:30, so the local hour boundary is not a UTC one.
		{"0 * * * *", "Asia/Kolkata", "2026-11-21T10:30:00Z"},
	} {
		s, err := Parse(c.expr, c.tz)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := s.Next(base).UTC().Format(time.RFC3339); got != c.want {
			t.Errorf("Next(%q, %s) = %s, want %s", c.expr, c.tz, got, c.want)
		}
	}
}

func TestScheduleDST(t *testing.T) {
	// New York springs forward on 2026-03-08: 02:30 local doesn't exist.
	s, err := Parse("30 2 * * *", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	got := s.Next(time.Date(2026, 3, 7, 12, 0, 0, 0, ny))
	if skipped := got.Day() == 9 || (got.Day() == 8 && got.Hour() == 3); !skipped {
		t.Fatalf("next = %s", got)
	}
	// Every run is strictly increasing across the fall-back hour.
	s, _ = Parse("*/30 * * * *", "America/New_York")
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	for i := 0; i < 12; i++ {
		n := s.Next(t0)
		if !n.After(t0) || n.Sub(t0) > time.Hour {
			t.Fatalf("step %d: %s -> %s", i, t0, n)
		}
		t0 = n
	}
}

func TestScheduleInvalid(t *testing.T) {
	for _, c := range [][2]string{
		{"* * * *", "UTC"}, {"60 * * * *", "UTC"}, {"* 24 * * *", "UTC"}, {"*/0 * * * *", "UTC"},
		{"5-1 * * * *", "UTC"}, {"0 0 31 2 *", "UTC"}, {"* * * * *", "Mars/Olympus"}, {"x * * * *", "UTC"},
	} {
		if _, err := Parse(c[0], c[1]); err == nil {
			t.Errorf("Parse(%q, %s) accepted", c[0], c[1])
		}
	}
}
