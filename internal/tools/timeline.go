package tools

// get_event_timeline: what a tracked object (or every object in an alert) did, as text.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

// maxTimelineObjects bounds how many objects of one alert get a timeline.
const maxTimelineObjects = 5

// objects is what an id stands for: a tracked object, or an alert and its objects.
type objects struct {
	ids    []string // tracked objects, at most maxTimelineObjects
	camera string
	what   string    // the object's label, or the alert's objects
	start  time.Time // when it began
	single *frigate.Event
}

// resolveObjects looks id up as a tracked object, then as an alert. Errors are ready for the
// model; tool names the calling tool in the log.
func (t *Toolset) resolveObjects(ctx context.Context, tool, id string) (objects, error) {
	var o objects
	id = strings.TrimSpace(id)
	if id == "" {
		return o, errors.New("id is required; get it from get_events, last_seen or get_alerts")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return o, unavailable(err)
	}
	notFound := fmt.Errorf("no tracked object or alert with id %q; get ids from get_events, last_seen or get_alerts", id)
	e, err := t.src.Event(ctx, id)
	switch {
	case err == nil:
		o = objects{ids: []string{e.ID}, camera: e.Camera, what: e.Label, start: e.Start.Time, single: e}
	case errors.Is(err, frigate.ErrNotFound):
		r, rerr := t.src.Review(ctx, id)
		if errors.Is(rerr, frigate.ErrNotFound) {
			return o, notFound
		}
		if rerr != nil {
			slog.Error(tool, "id", id, "err", rerr)
			return o, unavailable(rerr)
		}
		o = objects{ids: r.Data.Detections, camera: r.Camera, what: strings.Join(dedupe(r.Data.Objects), ", "), start: r.Start.Time}
		if len(o.ids) > maxTimelineObjects {
			o.ids = o.ids[:maxTimelineObjects]
		}
	default:
		slog.Error(tool, "id", id, "err", err)
		return o, unavailable(err)
	}
	if !cat.visible(o.camera) {
		return objects{}, notFound
	}
	if len(o.ids) == 0 {
		return o, fmt.Errorf("%s has no tracked objects to follow", id)
	}
	return o, nil
}

func (t *Toolset) eventTimeline(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	o, err := t.resolveObjects(ctx, "get_event_timeline", id)
	if err != nil {
		return nil, nil, err
	}
	ids, camera, what, start, single := o.ids, o.camera, o.what, o.start, o.single
	entries, err := t.src.Timeline(ctx, ids, 100*len(ids))
	if err != nil {
		slog.Error("get_event_timeline", "id", id, "err", err)
		return nil, nil, unavailable(err)
	}
	now := t.opt.Now().In(t.opt.Location)
	slog.Info("get_event_timeline", "id", id, "objects", len(ids), "entries", len(entries))
	if len(entries) == 0 {
		return text(fmt.Sprintf("Frigate has no timeline for %s (%s on %s at %s); it may have been cleaned up already.\n",
			id, what, camera, clock(start, now))), nil, nil
	}
	// Each object's top score: Frigate's best confidence over its whole life.
	scores := map[string]float64{}
	if single != nil {
		scores[single.ID] = single.Data.TopScore
	} else {
		for _, oid := range ids {
			if ev, err := t.src.Event(ctx, oid); err == nil {
				scores[oid] = ev.Data.TopScore
			}
		}
	}
	return text(describeTimeline(entries, camera, what, len(ids) > 1, scores, now)), nil, nil
}

// objectFacts sums up one object from its timeline, for telling a real one from a false
// detection: Frigate's top confidence, whether it appeared inside a zone or came in from
// outside, and whether it moved.
func objectFacts(entries []frigate.TimelineEntry, topScore float64) string {
	var facts []string
	if topScore > 0 {
		facts = append(facts, fmt.Sprintf("Frigate's top confidence %.0f%%", topScore*100))
	}
	var seen, entered time.Time
	var zones []string
	var centers [][2]float64
	for _, en := range entries {
		switch en.ClassType {
		case "visible":
			if seen.IsZero() {
				seen = en.Timestamp.Time
			}
		case "entered_zone":
			if entered.IsZero() {
				entered, zones = en.Timestamp.Time, dedupe(en.Data.Zones)
			}
		}
		if b := en.Data.Box; len(b) == 4 {
			centers = append(centers, [2]float64{b[0] + b[2]/2, b[1] + b[3]/2})
		}
	}
	switch {
	case entered.IsZero():
		facts = append(facts, "never entered a zone")
	case !seen.IsZero() && entered.Sub(seen) > time.Second:
		facts = append(facts, fmt.Sprintf("first seen outside its zones, entered %s after %s",
			strings.Join(zones, ", "), span(seen, &entered)))
	default:
		facts = append(facts, "appeared already inside "+strings.Join(zones, ", "))
	}
	if len(centers) >= 2 {
		moved := 0.0
		for _, c := range centers[1:] {
			moved = max(moved, math.Hypot(c[0]-centers[0][0], c[1]-centers[0][1]))
		}
		if moved >= movedFar {
			facts = append(facts, "moved across the frame")
		} else {
			facts = append(facts, "stayed in one place")
		}
	}
	return strings.Join(facts, "; ")
}

// movedFar is how far an object's center must get from where it was first seen (as a fraction
// of the frame) to count as having moved.
const movedFar = 0.05

// describeTimeline writes the entries as lines a person (and a small model) can follow. With
// several objects, each line says which.
func describeTimeline(entries []frigate.TimelineEntry, camera, what string, several bool, scores map[string]float64, now time.Time) string {
	first, last := entries[0].Timestamp.Time, entries[len(entries)-1].Timestamp.Time
	var b strings.Builder
	fmt.Fprintf(&b, "Timeline of %s on %s, %s (%s):\n", what, camera, clock(first, now), span(first, &last))
	// One line of facts per object, in the order they appeared.
	var order []string
	byObject := map[string][]frigate.TimelineEntry{}
	labels := map[string]string{}
	for _, en := range entries {
		if _, ok := byObject[en.SourceID]; !ok {
			order = append(order, en.SourceID)
		}
		byObject[en.SourceID] = append(byObject[en.SourceID], en)
		if labels[en.SourceID] == "" {
			labels[en.SourceID] = en.Data.Label
		}
	}
	for _, id := range order {
		facts := objectFacts(byObject[id], scores[id])
		if several {
			facts = labels[id] + ": " + facts
		}
		b.WriteString(capitalize(facts) + ".\n")
	}
	stillSince := map[string]time.Time{}
	for _, en := range entries {
		at := en.Timestamp.In(now.Location()).Format("15:04:05")
		who := ""
		if several {
			who = en.Data.Label
			if s := string(en.Data.SubLabel); s != "" {
				who += " (" + s + ")"
			}
			who += ": "
		}
		var line string
		switch en.ClassType {
		case "visible":
			line = en.Data.Label
			if s := string(en.Data.SubLabel); s != "" {
				line += " (" + s + ")"
			}
			line += " seen" + where(en.Data.Box)
			who = ""
		case "entered_zone":
			line = "entered " + strings.Join(dedupe(en.Data.Zones), ", ")
		case "stationary":
			line = "stopped moving"
			stillSince[en.SourceID] = en.Timestamp.Time
		case "active":
			line = "moving again"
			if since, ok := stillSince[en.SourceID]; ok {
				line += fmt.Sprintf(" (still for %s)", span(since, &en.Timestamp.Time))
				delete(stillSince, en.SourceID)
			}
		case "attribute":
			line = attribute(en.Data.Attribute) + " visible"
		case "gone":
			line = "gone, last seen" + strings.TrimPrefix(where(en.Data.Box), ",")
			if since, ok := stillSince[en.SourceID]; ok {
				line = fmt.Sprintf("gone after standing still for %s, last seen%s", span(since, &en.Timestamp.Time),
					strings.TrimPrefix(where(en.Data.Box), ","))
			}
		case "heard":
			line = "heard " + en.Data.Label
		default:
			line = strings.ReplaceAll(en.ClassType, "_", " ")
		}
		fmt.Fprintf(&b, "- %s %s%s\n", at, who, line)
	}
	return b.String()
}

// where says roughly where in the frame a box is: ", near the left edge", ", in the middle".
func where(box []float64) string {
	if len(box) != 4 {
		return ""
	}
	x, y := box[0]+box[2]/2, box[1]+box[3]/2
	switch {
	case x < 0.2:
		return ", near the left edge"
	case x > 0.8:
		return ", near the right edge"
	case y < 0.2:
		return ", near the top (far away)"
	case y > 0.8:
		return ", near the bottom (close by)"
	}
	return ", in the middle"
}

// attribute names what Frigate saw: "face", "license plate", or a logo.
func attribute(a any) string {
	var names []string
	switch v := a.(type) {
	case string:
		names = []string{v}
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok {
				names = append(names, s)
			}
		}
	}
	for i, n := range names {
		names[i] = strings.ReplaceAll(n, "_", " ")
	}
	if len(names) == 0 {
		return "something"
	}
	return strings.Join(dedupe(names), ", ")
}
