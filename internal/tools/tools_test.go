package tools

import (
	"testing"
	"time"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

var now = time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)

func TestWindow(t *testing.T) {
	midnight := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in            string
		after, before time.Time
		label         string
	}{
		{"", now.Add(-24 * time.Hour), now, "last 24 hours"},
		{"30m", now.Add(-30 * time.Minute), now, "last 30 minutes"},
		{"6 hours", now.Add(-6 * time.Hour), now, "last 6 hours"},
		{"last 2d", now.Add(-48 * time.Hour), now, "last 2 days"},
		{"1w", now.Add(-7 * 24 * time.Hour), now, "last 7 days"},
		{"90d", now.Add(-maxRange), now, "last 31 days"},
		{"Today", midnight, now, "today"},
		{"yesterday", midnight.AddDate(0, 0, -1), midnight, "yesterday"},
	} {
		after, before, label, err := window(tc.in, "24h", now)
		if err != nil || !after.Equal(tc.after) || !before.Equal(tc.before) || label != tc.label {
			t.Errorf("%q: got %v..%v %q %v", tc.in, after, before, label, err)
		}
	}
	for _, bad := range []string{"soon", "-2h", "0"} {
		if _, _, _, err := window(bad, "24h", now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClockAndSpan(t *testing.T) {
	for in, want := range map[time.Time]string{
		now.Add(-time.Hour):      "today 14:00",
		now.Add(-20 * time.Hour): "yesterday 19:00",
		now.Add(-72 * time.Hour): "Thu 29 Oct 15:00",
	} {
		if got := clock(in, now); got != want {
			t.Errorf("clock(%v) = %q, want %q", in, got, want)
		}
	}
	end := now.Add(75 * time.Second)
	if got := span(now, &end); got != "1m15s" {
		t.Errorf("span = %q", got)
	}
	if got := span(now, nil); got != "ongoing" {
		t.Errorf("span = %q", got)
	}
}

func TestCatalogMatching(t *testing.T) {
	on, off := true, false
	cfg := &frigate.Config{Cameras: map[string]frigate.CameraConfig{
		"front_door": {Enabled: &on},
		"Back-Yard":  {},
		"old":        {Enabled: &off},
	}}
	fd := cfg.Cameras["front_door"]
	fd.Objects.Track = []string{"person", "car", "bus"}
	cfg.Cameras["front_door"] = fd
	cat := buildCatalog(cfg, nil)
	for in, want := range map[string]string{"Front Door": "front_door", "back yard": "Back-Yard", "BACK_YARD": "Back-Yard"} {
		if got, err := cat.camera(in); err != nil || got != want {
			t.Errorf("camera(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := cat.camera("old"); err == nil {
		t.Error("disabled camera accepted")
	}
	for in, want := range map[string]string{"people": "person", "Cars": "car", "bus": "bus", "person": "person"} {
		if got, err := cat.label(in); err != nil || got != want {
			t.Errorf("label(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := cat.zone("porch"); err == nil || err.Error() != `unknown zone "porch"; no zones are configured` {
		t.Errorf("zone: %v", err)
	}
}
