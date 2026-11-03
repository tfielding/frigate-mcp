package tools

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// maxRange caps how far back a query may reach.
const maxRange = 31 * 24 * time.Hour

// window turns a "since" value into a time range ending now (or at midnight, for "yesterday").
// Accepted: durations like 30m, 6h, 2d, 1w; "today"; "yesterday". Empty means def.
func window(since, def string, now time.Time) (after, before time.Time, label string, err error) {
	s := strings.ToLower(strings.TrimSpace(since))
	if s == "" {
		s = def
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch s {
	case "today":
		return midnight, now, "today", nil
	case "yesterday":
		return midnight.AddDate(0, 0, -1), midnight, "yesterday", nil
	}
	d, err := parseDuration(strings.TrimPrefix(strings.TrimPrefix(s, "last "), "past "))
	if err != nil || d <= 0 {
		return time.Time{}, time.Time{}, "", fmt.Errorf("since %q not understood; use e.g. 30m, 6h, 2d, 1w, today or yesterday", since)
	}
	if d > maxRange {
		d = maxRange
	}
	return now.Add(-d), now, "last " + human(d), nil
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.ReplaceAll(s, " ", "")
	for _, u := range []struct {
		suffixes []string
		unit     time.Duration
	}{
		{[]string{"weeks", "week", "w"}, 7 * 24 * time.Hour},
		{[]string{"days", "day", "d"}, 24 * time.Hour},
		{[]string{"hours", "hour", "hrs", "hr"}, time.Hour},
		{[]string{"minutes", "minute", "mins", "min"}, time.Minute},
	} {
		for _, suf := range u.suffixes {
			if n, ok := strings.CutSuffix(s, suf); ok {
				f, err := strconv.ParseFloat(n, 64)
				if err != nil {
					return 0, err
				}
				return time.Duration(f * float64(u.unit)), nil
			}
		}
	}
	return time.ParseDuration(s)
}

func human(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0 && d >= 48*time.Hour:
		return plural(int(d/(24*time.Hour)), "day")
	case d%time.Hour == 0:
		return plural(int(d/time.Hour), "hour")
	default:
		return plural(int(d.Round(time.Minute)/time.Minute), "minute")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// clock formats a time for a person: "today 14:05", "yesterday 02:14" or "Mon 26 Oct 09:30".
func clock(t, now time.Time) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "today " + t.Format("15:04")
	case t.Before(now) && sameDay(t, now.AddDate(0, 0, -1)):
		return "yesterday " + t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}

func sameDay(a, b time.Time) bool {
	y1, m1, d1 := a.Date()
	y2, m2, d2 := b.Date()
	return y1 == y2 && m1 == m2 && d1 == d2
}

// span formats how long something lasted; end nil means still going.
func span(start time.Time, end *time.Time) string {
	if end == nil {
		return "ongoing"
	}
	d := end.Sub(start).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// timeRange is the time arguments every list and grid tool takes: since (a duration back from
// the end), or from/to as times of day or dates.
type timeRange struct{ since, from, to string }

const timeHelp = "use e.g. 11:30, 2pm, noon, yesterday 18:00 or 2026-10-30 09:15"

// resolve turns the arguments into [after, before). def is the default since ("" = no start).
// With neither from nor to it's the same as window. Otherwise to (default now) is the end, and from
// the start, or else since (or def) counted back from to.
func (r timeRange) resolve(def string, now time.Time) (after, before time.Time, label string, err error) {
	if strings.TrimSpace(r.from) == "" && strings.TrimSpace(r.to) == "" {
		if r.since == "" && def == "" {
			return time.Time{}, time.Time{}, "all retained history", nil
		}
		return window(r.since, def, now)
	}
	before = now
	if r.to != "" {
		if before, err = parseTimeArg(r.to, now); err != nil {
			return time.Time{}, time.Time{}, "", fmt.Errorf("to %q not understood; %s", r.to, timeHelp)
		}
		before = minTime(before, now)
	}
	switch {
	case r.from != "":
		if after, err = parseTimeArg(r.from, now); err != nil {
			return time.Time{}, time.Time{}, "", fmt.Errorf("from %q not understood; %s", r.from, timeHelp)
		}
		if r.to == "" && after.After(now) {
			return time.Time{}, time.Time{}, "", fmt.Errorf("from %s is in the future", r.from)
		}
	case r.since != "" || def != "":
		if after, _, _, err = window(r.since, def, before); err != nil {
			return time.Time{}, time.Time{}, "", err
		}
	}
	if !after.IsZero() && !after.Before(before) {
		return time.Time{}, time.Time{}, "", fmt.Errorf("from (%s) must be before to (%s)", clock(after, now), clock(before, now))
	}
	if !after.IsZero() && before.Sub(after) > maxRange {
		after = before.Add(-maxRange)
	}
	switch {
	case after.IsZero():
		label = "up to " + clock(before, now)
	case before.Equal(now):
		label = "since " + clock(after, now)
	default:
		label = "between " + clock(after, now) + " and " + clock(before, now)
	}
	return after, before, label, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// parseTimeArg reads a point in time the way people say it: "11:30", "2pm", "2:15 pm", "noon",
// "midnight", "14:05:30", optionally after "today", "yesterday" or a date (2026-10-30). Without a
// day it's the most recent such time: at 10:00, "11:30" means yesterday 11:30.
func parseTimeArg(s string, now time.Time) (time.Time, error) {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	day, explicit := now, false
	if rest, ok := strings.CutPrefix(s, "today"); ok {
		s, explicit = strings.TrimSpace(strings.TrimPrefix(rest, " at")), true
	} else if rest, ok := strings.CutPrefix(s, "yesterday"); ok {
		s, day, explicit = strings.TrimSpace(strings.TrimPrefix(rest, " at")), now.AddDate(0, 0, -1), true
	} else if date, rest, ok := strings.Cut(s, " "); ok {
		if d, err := time.ParseInLocation("2006-01-02", date, now.Location()); err == nil {
			s, day, explicit = strings.TrimSpace(strings.TrimPrefix(rest, "at ")), d, true
		}
	}
	var h, m, sec int
	switch s {
	case "noon", "midday":
		h = 12
	case "midnight", "":
		if s == "" && !explicit {
			return time.Time{}, errors.New("empty time")
		}
	default:
		ampm := ""
		for _, suf := range []string{"a.m.", "p.m.", "am", "pm"} {
			if rest, ok := strings.CutSuffix(s, suf); ok {
				s, ampm = strings.TrimSpace(rest), suf[:1]
				break
			}
		}
		parts := strings.Split(s, ":")
		if len(parts) > 3 || (ampm == "" && len(parts) == 1) {
			return time.Time{}, fmt.Errorf("bad time %q", s)
		}
		vals := make([]int, 3)
		for i, p := range parts {
			v, err := strconv.Atoi(p)
			if err != nil || v < 0 || (i > 0 && (len(p) != 2 || v > 59)) {
				return time.Time{}, fmt.Errorf("bad time %q", s)
			}
			vals[i] = v
		}
		h, m, sec = vals[0], vals[1], vals[2]
		switch {
		case ampm != "" && (h < 1 || h > 12):
			return time.Time{}, fmt.Errorf("bad hour %d", h)
		case ampm == "p" && h < 12:
			h += 12
		case ampm == "a" && h == 12:
			h = 0
		case h > 23:
			return time.Time{}, fmt.Errorf("bad hour %d", h)
		}
	}
	t := time.Date(day.Year(), day.Month(), day.Day(), h, m, sec, 0, now.Location())
	if !explicit && t.After(now) {
		t = t.AddDate(0, 0, -1)
	}
	return t, nil
}

// timeArg formats t so parseTimeArg reads it back exactly: "12:28:40", "yesterday 12:28:40" or
// "2026-10-30 12:28:40". Used to tell the model how to ask for older items.
func timeArg(t, now time.Time) string {
	t = t.In(now.Location())
	switch {
	case sameDay(t, now):
		return t.Format("15:04:05")
	case sameDay(t, now.AddDate(0, 0, -1)):
		return "yesterday " + t.Format("15:04:05")
	}
	return t.Format("2006-01-02 15:04:05")
}
