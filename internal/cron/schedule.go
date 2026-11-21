// Package cron schedules and runs cron jobs as one-off containers.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed five-field cron expression in a time zone.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // bit sets
	domAny, dowAny                bool
	loc                           *time.Location
}

type field struct {
	name     string
	min, max int
	names    map[string]int
}

var (
	fMinute = field{name: "minute", min: 0, max: 59}
	fHour   = field{name: "hour", min: 0, max: 23}
	fDom    = field{name: "day of month", min: 1, max: 31}
	fMonth  = field{name: "month", min: 1, max: 12, names: map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}}
	fDow = field{name: "day of week", min: 0, max: 7, names: map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}}
)

var macros = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

// Parse parses "minute hour day-of-month month day-of-week" (or a macro such
// as @daily) in the named IANA time zone. Like Vixie cron, when both
// day-of-month and day-of-week are restricted a day matching either runs.
func Parse(expr, tz string) (Schedule, error) {
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Schedule{}, fmt.Errorf("unknown time zone %q", tz)
	}
	expr = strings.TrimSpace(expr)
	if m, ok := macros[strings.ToLower(expr)]; ok {
		expr = m
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Schedule{}, errors.New("a schedule has 5 fields: minute hour day-of-month month day-of-week (e.g. */5 * * * *)")
	}
	s := Schedule{loc: loc, domAny: parts[2] == "*", dowAny: parts[4] == "*"}
	for i, f := range []field{fMinute, fHour, fDom, fMonth, fDow} {
		bits, err := f.parse(parts[i])
		if err != nil {
			return Schedule{}, err
		}
		switch i {
		case 0:
			s.minute = bits
		case 1:
			s.hour = bits
		case 2:
			s.dom = bits
		case 3:
			s.month = bits
		case 4:
			if bits&(1<<7) != 0 { // 7 is Sunday too
				bits |= 1
			}
			s.dow = bits
		}
	}
	if s.Next(time.Now()).IsZero() {
		return Schedule{}, errors.New("this schedule never runs")
	}
	return s, nil
}

func (f field) value(s string) (int, error) {
	if v, ok := f.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < f.min || v > f.max {
		return 0, fmt.Errorf("%s: %q is not in %d-%d", f.name, s, f.min, f.max)
	}
	return v, nil
}

func (f field) parse(spec string) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(spec, ",") {
		rng, stepStr, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n < 1 || n > f.max+1 {
				return 0, fmt.Errorf("%s: bad step %q", f.name, stepStr)
			}
			step = n
		}
		lo, hi := f.min, f.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = f.value(a); err != nil {
				return 0, err
			}
			if hi, err = f.value(b); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("%s: range %q is backwards", f.name, rng)
			}
		default:
			v, err := f.value(rng)
			if err != nil {
				return 0, err
			}
			lo = v
			if !hasStep {
				hi = v
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func has(bits uint64, v int) bool { return v >= 0 && v < 64 && bits&(1<<v) != 0 }

func (s Schedule) dayMatches(t time.Time) bool {
	d, w := has(s.dom, t.Day()), has(s.dow, int(t.Weekday()))
	switch {
	case s.domAny && s.dowAny:
		return true
	case s.domAny:
		return w
	case s.dowAny:
		return d
	default:
		return d || w
	}
}

// Next returns the first run strictly after t, or the zero time if there is
// none within five years.
func (s Schedule) Next(t time.Time) time.Time {
	// Zone offsets are whole minutes, so truncating the instant is exact.
	t = t.In(s.loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	// Jumps are built from local fields; in a repeated (DST fall-back) hour
	// time.Date may resolve to an earlier instant, so never move backwards.
	jump := func(next time.Time) {
		if !next.After(t) {
			next = t.Add(time.Minute)
		}
		t = next
	}
	for t.Before(limit) {
		switch {
		case !has(s.month, int(t.Month())):
			jump(time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, s.loc))
		case !s.dayMatches(t):
			jump(time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, s.loc))
		case !has(s.hour, t.Hour()):
			jump(time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, s.loc))
		case !has(s.minute, t.Minute()):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// Location is the schedule's time zone.
func (s Schedule) Location() *time.Location { return s.loc }
