package tools

import (
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
