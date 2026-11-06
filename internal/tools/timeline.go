package tools

// get_event_timeline: what a tracked object (or every object in an alert) did, as text.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

// maxTimelineObjects bounds how many objects of one alert get a timeline.
const maxTimelineObjects = 5

func (t *Toolset) eventTimeline(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, errors.New("id is required; get it from get_events, last_seen or get_alerts")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	// An id is a tracked object's or an alert's: an alert's timeline is that of its objects.
	var ids []string
	var camera, what string
	var start time.Time
	e, err := t.src.Event(ctx, id)
	switch {
	case err == nil:
		ids, camera, what, start = []string{e.ID}, e.Camera, e.Label, e.Start.Time
	case errors.Is(err, frigate.ErrNotFound):
		r, rerr := t.src.Review(ctx, id)
		if errors.Is(rerr, frigate.ErrNotFound) {
			return nil, nil, fmt.Errorf("no tracked object or alert with id %q; get ids from get_events, last_seen or get_alerts", id)
		}
		if rerr != nil {
			slog.Error("get_event_timeline", "id", id, "err", rerr)
			return nil, nil, unavailable(rerr)
		}
		ids, camera, what, start = r.Data.Detections, r.Camera, strings.Join(dedupe(r.Data.Objects), ", "), r.Start.Time
		if len(ids) > maxTimelineObjects {
			ids = ids[:maxTimelineObjects]
		}
	default:
		slog.Error("get_event_timeline", "id", id, "err", err)
		return nil, nil, unavailable(err)
	}
	if !cat.visible(camera) {
		return nil, nil, fmt.Errorf("no tracked object or alert with id %q; get ids from get_events, last_seen or get_alerts", id)
	}
	if len(ids) == 0 {
		return nil, nil, fmt.Errorf("%s has no tracked objects to follow", id)
	}
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
	return text(describeTimeline(entries, camera, what, len(ids) > 1, now)), nil, nil
}

// describeTimeline writes the entries as lines a person (and a small model) can follow. With
// several objects, each line says which.
func describeTimeline(entries []frigate.TimelineEntry, camera, what string, several bool, now time.Time) string {
	first, last := entries[0].Timestamp.Time, entries[len(entries)-1].Timestamp.Time
	var b strings.Builder
	fmt.Fprintf(&b, "Timeline of %s on %s, %s (%s):\n", what, camera, clock(first, now), span(first, &last))
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
