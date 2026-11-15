package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

func at(h, m int) frigate.Time {
	return frigate.Time{Time: time.Date(2026, 11, 1, h, m, 0, 0, time.UTC)}
}

func rev(id, cam, sev string, start frigate.Time, objects []string, threat int) frigate.Review {
	r := frigate.Review{ID: id, Camera: cam, Severity: sev, Start: start}
	r.Data.Objects = objects
	if threat > 0 {
		r.Data.Metadata = &frigate.ReviewMetadata{ThreatLevel: threat}
	}
	return r
}

func ev(id, cam, label string, start frigate.Time, name, plate string) frigate.Event {
	e := frigate.Event{ID: id, Camera: cam, Label: label, Start: start, SubLabel: frigate.SubLabel(name)}
	e.Data.Plate = plate
	return e
}

func TestDigestEmpty(t *testing.T) {
	d := digest{period: "last 24 hours", where: "all cameras", now: now, cameras: []string{"a", "b"}}
	want := "Digest for last 24 hours, all cameras. Times are UTC.\nNo alerts, detections or tracked objects.\n"
	if got := d.text(); got != want {
		t.Errorf("got %q", got)
	}
}

func TestDigest(t *testing.T) {
	d := digest{
		period: "last 24 hours", where: "all cameras", now: now,
		cameras: []string{"backyard", "driveway", "front_door"},
		reviews: []frigate.Review{
			rev("r1", "driveway", "alert", at(2, 15), []string{"person"}, 0), // overnight
			rev("r2", "front_door", "alert", at(11, 0), []string{"person"}, 1),
			rev("r3", "driveway", "detection", at(14, 0), []string{"car"}, 0),
			rev("r4", "attic", "alert", at(3, 0), []string{"person"}, 2), // not a covered camera
		},
		events: []frigate.Event{
			ev("e1", "driveway", "car", at(14, 5), "", "ABC123"),
			ev("e2", "driveway", "car", at(14, 20), "", ""),
			ev("e3", "driveway", "car", at(14, 40), "", ""),
			ev("e4", "front_door", "person", at(11, 0), "Alex", ""),
			ev("e5", "attic", "person", at(3, 0), "", ""),
		},
	}
	got := d.text()
	for _, s := range []string{
		"Digest for last 24 hours, all cameras. Times are UTC.\n",
		"Alerts: 2, detections: 1.\n",
		"Tracked objects: 4 (car 3, person 1).\n",
		"By camera, busiest first:\n- driveway: 1 alert, 1 detection; tracked: car 3\n- front_door: 1 alert; tracked: person 1\n",
		"No activity: backyard.\n",
		"Busiest hour: today 14:00-15:00 (3 objects).\n",
		"Recognized: Alex (1).\n",
		"Plates: ABC123 (1).\n",
		"- today 11:00 (ongoing), front_door: alert, person; id r2\n  Why: Frigate rated it suspicious.\n",
		"- today 02:15 (ongoing), driveway: alert, person; zones: driveway; id r1\n  Why: overnight.\n",
	} {
		if s == "- today 02:15 (ongoing), driveway: alert, person; zones: driveway; id r1\n  Why: overnight.\n" {
			// r1 has no zones in this fixture.
			s = "- today 02:15 (ongoing), driveway: alert, person; id r1\n  Why: overnight.\n"
		}
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "attic") || strings.Contains(got, "r4") {
		t.Errorf("uncovered camera shown:\n%s", got)
	}
	// The suspicious alert outranks the overnight one.
	if strings.Index(got, "id r2") > strings.Index(got, "id r1") {
		t.Errorf("not most notable first:\n%s", got)
	}
	// A detection that nothing flagged is counted but not listed.
	if strings.Contains(got, "id r3") {
		t.Errorf("routine detection listed:\n%s", got)
	}
}

func TestDigestSingleCameraAndTruncated(t *testing.T) {
	d := digest{
		period: "today", where: "driveway", now: now, cameras: []string{"driveway"}, truncated: true,
		reviews: []frigate.Review{rev("r1", "driveway", "alert", at(12, 0), []string{"car"}, 0)},
		events:  []frigate.Event{ev("e1", "driveway", "car", at(12, 0), "", ""), ev("e2", "front_door", "car", at(12, 0), "", "")},
	}
	got := d.text()
	if strings.Contains(got, "By camera") || strings.Contains(got, "front_door") ||
		!strings.Contains(got, "Tracked objects: at least 1 (car 1).\n") ||
		!strings.Contains(got, "Nothing stood out: no overnight alerts") {
		t.Errorf("got:\n%s", got)
	}
}

func TestOvernightAndLists(t *testing.T) {
	for h, want := range map[int]bool{22: false, 23: true, 0: true, 5: true, 6: false, 12: false} {
		if got := overnight(time.Date(2026, 11, 1, h, 30, 0, 0, time.UTC)); got != want {
			t.Errorf("overnight(%d:30) = %v", h, got)
		}
	}
	m := map[string]int{"b": 2, "a": 2, "c": 5, "d": 1}
	if got := countList(m, false, 0); got != "c 5, a 2, b 2, d 1" {
		t.Errorf("countList = %q", got)
	}
	if got := countList(m, true, 2); got != "c (5), a (2) and 2 more" {
		t.Errorf("countList limited = %q", got)
	}
}
