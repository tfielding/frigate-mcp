package main

import (
	"strings"
	"testing"

	"github.com/tfielding/frigate-mcp/internal/tools"
)

func TestDailyDigest(t *testing.T) {
	cs := start(t, config{}, nil)
	res := call(t, cs, "daily_digest", nil)
	got := text(res)
	if res.IsError {
		t.Fatalf("error: %s", got)
	}
	for _, s := range []string{
		"Digest for last 24 hours, all cameras. Times are UTC.\n",
		"Alerts: 3, detections: 1.\n",
		"Tracked objects: 2 (car 1, person 1).\n",
		"- driveway: 1 alert, 1 detection; tracked: car 1\n",
		"- front_door: 1 alert; tracked: person 1\n",
		"- bedroom: 1 alert\n",
		"Plates: ABC123 (1).\n",
		"Worth a look, most notable first:\n",
		"- today 02:15 (36s), driveway: alert, person; zones: driveway; id r-alert1\n  Why: overnight.\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "id r-detect") {
		t.Errorf("routine detection listed:\n%s", got)
	}

	// One camera: the other cameras' alerts and objects are left out.
	got = text(call(t, cs, "daily_digest", map[string]any{"camera": "Driveway"}))
	for _, s := range []string{
		"Digest for last 24 hours, driveway. Times are UTC.\n",
		"Alerts: 1, detections: 1.\n",
		"Tracked objects: 1 (car 1).\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("driveway: missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "front_door") || strings.Contains(got, "By camera") {
		t.Errorf("driveway digest shows other cameras:\n%s", got)
	}

	res = call(t, cs, "daily_digest", map[string]any{"camera": "attic"})
	if !res.IsError || !strings.Contains(text(res), `unknown camera "attic"`) {
		t.Errorf("got %s", text(res))
	}
	res = call(t, cs, "daily_digest", map[string]any{"since": "sometime"})
	if !res.IsError || !strings.Contains(text(res), "use e.g. 30m, 6h") {
		t.Errorf("got %s", text(res))
	}
}

func TestDailyDigestHonorsAllowlist(t *testing.T) {
	cs := start(t, config{opt: tools.Options{Allowed: map[string]bool{"driveway": true}}}, nil)
	got := text(call(t, cs, "daily_digest", nil))
	if strings.Contains(got, "bedroom") || strings.Contains(got, "front_door") || strings.Contains(got, "r-hidden") ||
		!strings.Contains(got, "Alerts: 1, detections: 1.\n") || !strings.Contains(got, "Tracked objects: 1 (car 1).\n") {
		t.Fatalf("got:\n%s", got)
	}
}
