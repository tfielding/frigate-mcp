// Package tools defines the MCP tools. Everything here is read-only.
package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

// Source is the Frigate backend (implemented by frigate.Client).
type Source interface {
	Config(ctx context.Context) (*frigate.Config, error)
	Reviews(ctx context.Context, f frigate.Filter, severity string) ([]frigate.Review, error)
	Review(ctx context.Context, id string) (*frigate.Review, error)
	Events(ctx context.Context, f frigate.Filter) ([]frigate.Event, error)
	Event(ctx context.Context, id string) (*frigate.Event, error)
	Search(ctx context.Context, query string, f frigate.Filter) ([]frigate.Event, error)
	EventSnapshot(ctx context.Context, id string, height int) ([]byte, error)
	RecordingFrame(ctx context.Context, camera string, t float64, height int) ([]byte, error)
	Stats(ctx context.Context) (*frigate.Stats, error)
}

type Options struct {
	Allowed         map[string]bool // lowercased camera names; empty = all cameras
	SnapshotHeight  int             // image height in pixels; 0 = native
	GridCamera      bool            // show the camera name on get_alerts_grid tiles
	GridBoxes       bool            // get_alerts_grid prefers object snapshots with boxes over recorded frames
	GridLabelCorner Corner          // where grid tile labels go; empty = bottom-left
	Location        *time.Location  // time zone for times shown to the model
	Now             func() time.Time
}

const (
	defaultLimit = 10
	maxLimit     = 50
	searchLimit  = 5
	maxSearch    = 20
	configTTL    = time.Minute
)

type AlertsInput struct {
	Severity string `json:"severity,omitempty" jsonschema:"alert (default), detection, or all. Alerts are the important items; detections are lower priority"`
	Camera   string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Since    string `json:"since,omitempty" jsonschema:"how far back: e.g. 30m, 6h, 2d, 1w, today, yesterday. Default 24h. For a time of day use from and to instead"`
	From     string `json:"from,omitempty" jsonschema:"start of a time range, e.g. 11:30, 2pm, noon, yesterday 18:00. For \"around noon\" use from 11:30 and to 12:30"`
	To       string `json:"to,omitempty" jsonschema:"end of a time range, same forms as from. Default now"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum items to list, newest first. Default 10"`
}

type IDInput struct {
	ID string `json:"id" jsonschema:"the id shown in the list"`
}

type EventsInput struct {
	Camera string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Label  string `json:"label,omitempty" jsonschema:"only this kind of object, e.g. person, car, dog"`
	Zone   string `json:"zone,omitempty" jsonschema:"only objects that entered this zone"`
	Since  string `json:"since,omitempty" jsonschema:"how far back: e.g. 30m, 6h, 2d, 1w, today, yesterday. Default 24h. For a time of day use from and to instead"`
	From   string `json:"from,omitempty" jsonschema:"start of a time range, e.g. 11:30, 2pm, noon, yesterday 18:00. For \"around noon\" use from 11:30 and to 12:30"`
	To     string `json:"to,omitempty" jsonschema:"end of a time range, same forms as from. Default now"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum objects to list, newest first. Default 10"`
}

type SearchInput struct {
	Query  string `json:"query" jsonschema:"what to look for, in plain words, e.g. red pickup truck or person carrying a box"`
	Camera string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Since  string `json:"since,omitempty" jsonschema:"how far back: e.g. 6h, 2d, 1w. Default: all retained history"`
	From   string `json:"from,omitempty" jsonschema:"start of a time range, e.g. 11:30, 2pm, noon, yesterday 18:00. For \"around noon\" use from 11:30 and to 12:30"`
	To     string `json:"to,omitempty" jsonschema:"end of a time range, same forms as from. Default now"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum results, best match first. Default 5"`
}

// Toolset holds the tools and keeps search_events registered only while semantic search is on.
type Toolset struct {
	src     Source
	opt     Options
	cat     *catalogCache
	server  *mcp.Server
	mu      sync.Mutex
	search  bool
	refresh time.Duration
}

// Register adds the tools to s. search_events is added when Frigate reports semantic search as
// enabled; call Watch to keep that in sync while running.
func Register(ctx context.Context, s *mcp.Server, src Source, opt Options) *Toolset {
	if opt.Location == nil {
		opt.Location = time.Local
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	t := &Toolset{src: src, opt: opt, server: s, refresh: configTTL,
		cat: &catalogCache{src: src, allowed: opt.Allowed, ttl: configTTL}}

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_alerts",
		Description: "Text only, no pictures. Summarize recent activity Frigate flagged on the cameras: how many alerts and " +
			"detections, and a list (newest first) with time, camera, what was seen, zones and an id. Use for questions like " +
			"\"anything happen today?\" or \"how many alerts?\". To show or see pictures of alerts, use get_alerts_grid instead.",
		Annotations: readOnly("Frigate alerts"),
	}, t.alerts)
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_alerts_grid",
		Description: "Show pictures of recent alerts: one image with the newest alerts as a numbered grid (up to 9), plus " +
			"a matching list of times, cameras, what was seen and ids. Use whenever the user wants to see or be shown alerts, " +
			"e.g. \"show me today's alerts\" or \"what did the cameras see?\". Takes the same filters as get_alerts.",
		Annotations: readOnly("Alerts picture grid"),
	}, t.alertsGrid)
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_alert_snapshot",
		Description: "Show the picture (JPEG) for one alert or detection, by its id from get_alerts or get_alerts_grid. " +
			"For several alerts at once, use get_alerts_grid.",
		Annotations: readOnly("Alert snapshot"),
	}, t.alertSnapshot)
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_events",
		Description: "Text only, no pictures. List individual objects Frigate tracked (person, car, dog, ...), newest first, " +
			"with time, camera, zones and an id. Use for questions like \"when was the last car in the driveway?\"; for a time of " +
			"day like \"around noon\", pass from and to. " +
			"To show or see pictures of them, use get_events_grid instead.",
		Annotations: readOnly("Frigate events"),
	}, t.events)
	mcp.AddTool(s, &mcp.Tool{
		Name: "get_events_grid",
		Description: "Show pictures of tracked objects (person, car, dog, ...): one image with the newest matching objects as " +
			"a numbered grid (up to 9), each with its box drawn, plus a matching list with times and ids. Use whenever the " +
			"user wants to see objects, e.g. \"show me all the cars today\". Takes the same filters as get_events.",
		Annotations: readOnly("Events picture grid"),
	}, t.eventsGrid)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_event_snapshot",
		Description: "Show the saved snapshot (JPEG) of one tracked object from get_events or search_events, with its box drawn.",
		Annotations: readOnly("Event snapshot"),
	}, t.eventSnapshot)

	mcp.AddTool(s, &mcp.Tool{
		Name: "get_system_status",
		Description: "Check whether Frigate and the cameras are working: cameras that are offline or have a poor " +
			"connection, detector speed, free disk space, and the Frigate version. Use for questions like " +
			"\"are all the cameras working?\" or \"is the NVR running out of space?\".",
		Annotations: readOnly("Frigate system status"),
	}, t.systemStatus)

	start, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	t.sync(start)
	return t
}

// Watch re-reads Frigate's config periodically until ctx ends, adding or removing search_events.
func (t *Toolset) Watch(ctx context.Context) {
	tick := time.NewTicker(t.refresh)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.sync(ctx)
		}
	}
}

func (t *Toolset) sync(ctx context.Context) {
	cat, err := t.cat.get(ctx)
	if err != nil {
		slog.Warn("frigate config not available", "err", err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case cat.semantic && !t.search:
		mcp.AddTool(t.server, &mcp.Tool{
			Name: "search_events",
			Description: "Text only, no pictures. Search tracked objects by what they look like, in plain words " +
				"(\"red pickup truck\", \"person with a dog\"). Returns the best matches with time, camera and an id. " +
				"To show or see pictures of the matches, use search_events_grid instead.",
			Annotations: readOnly("Search events"),
		}, t.searchEvents)
		mcp.AddTool(t.server, &mcp.Tool{
			Name: "search_events_grid",
			Description: "Show pictures of tracked objects found by what they look like, in plain words (\"red pickup truck\", " +
				"\"person with a dog\"): one image with the best matches as a numbered grid (up to 9), each with its box " +
				"drawn, plus a matching list with times and ids. Use whenever the user wants to see the matches.",
			Annotations: readOnly("Search picture grid"),
		}, t.searchGrid)
		t.search = true
		slog.Info("semantic search enabled in frigate: search_events and search_events_grid available")
	case !cat.semantic && t.search:
		t.server.RemoveTools("search_events", "search_events_grid")
		t.search = false
		slog.Info("semantic search disabled in frigate: search_events and search_events_grid removed")
	}
}

func readOnly(title string) *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, DestructiveHint: &f, OpenWorldHint: &f}
}

func (t *Toolset) alerts(ctx context.Context, _ *mcp.CallToolRequest, in AlertsInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectReviews(ctx, in.Severity, in.Camera, timeRange{in.Since, in.From, in.To})
	if err != nil {
		return nil, nil, err
	}
	shown := sel.shown
	limit := clamp(in.Limit, defaultLimit, maxLimit)

	var b strings.Builder
	b.WriteString(sel.header())
	if len(shown) == 0 {
		fmt.Fprintf(&b, "Nothing to list.\n")
	} else {
		kind := map[string]string{"alert": "Alerts", "detection": "Detections", "all": "Alerts and detections"}[sel.severity]
		if len(shown) > limit {
			fmt.Fprintf(&b, "%s (newest %d of %d):\n", kind, limit, len(shown))
			shown = shown[:limit]
		} else {
			fmt.Fprintf(&b, "%s (newest first):\n", kind)
		}
		for _, r := range shown {
			b.WriteString(reviewLine(r, sel.severity == "all", sel.now))
		}
		if len(sel.shown) > limit {
			b.WriteString(olderHint(shown[len(shown)-1].Start.Time, sel.now))
		}
		b.WriteString("Use get_alert_snapshot with an id to show the picture, or get_alerts_grid to see several at once.\n")
	}
	slog.Info("get_alerts", "severity", sel.severity, "period", sel.period, "alerts", sel.alerts, "detections", sel.detections, "listed", len(shown))
	return text(b.String()), nil, nil
}

// selectReviews runs the review query behind get_alerts and get_alerts_grid: counts of both
// severities over the window, and the items of the requested severity, newest first.
func (t *Toolset) selectReviews(ctx context.Context, severityArg, camera string, tr timeRange) (reviewSelection, error) {
	var sel reviewSelection
	cat, err := t.cat.get(ctx)
	if err != nil {
		return sel, unavailable(err)
	}
	switch strings.ToLower(strings.TrimSpace(severityArg)) {
	case "", "alert", "alerts":
		sel.severity = "alert"
	case "detection", "detections":
		sel.severity = "detection"
	case "all", "both", "any":
		sel.severity = "all"
	default:
		return sel, fmt.Errorf("severity %q not understood; use alert, detection or all", severityArg)
	}
	sel.now = t.opt.Now().In(t.opt.Location)
	after, before, period, err := tr.resolve("24h", sel.now)
	if err != nil {
		return sel, err
	}
	sel.period = period
	cams, err := cat.filterCameras(camera, len(t.opt.Allowed) > 0)
	if err != nil {
		return sel, err
	}
	sel.where = "all cameras"
	if camera != "" {
		sel.where = cams[0]
	}
	// One query for both severities, so the counts cover everything even when only alerts are listed.
	items, err := t.src.Reviews(ctx, frigate.Filter{Cameras: cams, After: after, Before: before}, "")
	if err != nil {
		slog.Error("review query", "err", err)
		return sel, unavailable(err)
	}
	for _, r := range items {
		if !cat.visible(r.Camera) {
			continue
		}
		if r.Severity == "alert" {
			sel.alerts++
		} else {
			sel.detections++
		}
		if sel.severity == "all" || r.Severity == sel.severity {
			sel.shown = append(sel.shown, r)
		}
	}
	return sel, nil
}

func reviewLine(r frigate.Review, showSeverity bool, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s (%s), %s: ", clock(r.Start.Time, now), span(r.Start.Time, endTime(r.End)), r.Camera)
	if showSeverity {
		b.WriteString(r.Severity + ", ")
	}
	what := strings.Join(dedupe(r.Data.Objects), ", ")
	if what == "" {
		what = "activity"
	}
	b.WriteString(what)
	if subs := dedupe(r.Data.SubLabels); len(subs) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(subs, ", "))
	}
	if z := dedupe(r.Data.Zones); len(z) > 0 {
		fmt.Fprintf(&b, "; zones: %s", strings.Join(z, ", "))
	}
	if a := dedupe(r.Data.Audio); len(a) > 0 {
		fmt.Fprintf(&b, "; heard: %s", strings.Join(a, ", "))
	}
	fmt.Fprintf(&b, "; id %s\n", r.ID)
	if m := r.Data.Metadata; m != nil && (m.Title != "" || m.ShortSummary != "") {
		fmt.Fprintf(&b, "  Frigate's description: %s", strings.TrimSuffix(m.Title, "."))
		if m.ShortSummary != "" {
			fmt.Fprintf(&b, ". %s", m.ShortSummary)
		}
		if m.ThreatLevel > 0 {
			fmt.Fprintf(&b, " [%s]", map[int]string{1: "suspicious", 2: "critical"}[m.ThreatLevel])
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (t *Toolset) alertSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, errors.New("id is required; get it from get_alerts")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	r, err := t.src.Review(ctx, id)
	if errors.Is(err, frigate.ErrNotFound) || (err == nil && !cat.visible(r.Camera)) {
		return nil, nil, fmt.Errorf("no alert or detection with id %q; get ids from get_alerts", id)
	}
	if err != nil {
		slog.Error("get_alert_snapshot", "id", id, "err", err)
		return nil, nil, unavailable(err)
	}
	jpeg, err := t.reviewPicture(ctx, r, t.opt.SnapshotHeight, false)
	if err != nil {
		slog.Warn("get_alert_snapshot: no image", "id", id, "err", err)
		return nil, nil, fmt.Errorf("no picture available for %s; recordings and snapshots may be off for camera %s", id, r.Camera)
	}
	now := t.opt.Now().In(t.opt.Location)
	what := strings.Join(dedupe(r.Data.Objects), ", ")
	slog.Info("get_alert_snapshot", "id", id, "camera", r.Camera, "bytes", len(jpeg))
	return imageResult(fmt.Sprintf("%s on %s at %s: %s", capitalize(r.Severity), r.Camera, clock(r.Start.Time, now), what), jpeg), nil, nil
}

// maxSnapshotTries bounds how many of a review item's objects are tried for a snapshot, so an item
// with many objects on a camera without snapshots doesn't cost a request each.
const maxSnapshotTries = 3

// reviewPicture returns a picture of a review item: the recorded frame Frigate uses as its
// thumbnail, or the snapshot of one of its objects (with its box drawn by Frigate). snapshotFirst
// picks which is tried first; the other is the fallback.
func (t *Toolset) reviewPicture(ctx context.Context, r *frigate.Review, height int, snapshotFirst bool) ([]byte, error) {
	err := errors.New("no thumbnail time or objects")
	frame := func() []byte {
		if r.Data.ThumbTime <= 0 {
			return nil
		}
		var jpeg []byte
		if jpeg, err = t.src.RecordingFrame(ctx, r.Camera, r.Data.ThumbTime, height); err != nil {
			return nil
		}
		return jpeg
	}
	snapshot := func() []byte {
		for i, ev := range r.Data.Detections {
			if i == maxSnapshotTries {
				break
			}
			var jpeg []byte
			if jpeg, err = t.src.EventSnapshot(ctx, ev, height); err == nil {
				return jpeg
			}
		}
		return nil
	}
	order := []func() []byte{frame, snapshot}
	if snapshotFirst {
		order = []func() []byte{snapshot, frame}
	}
	for _, get := range order {
		if jpeg := get(); jpeg != nil {
			return jpeg, nil
		}
	}
	return nil, err
}

func (t *Toolset) events(ctx context.Context, _ *mcp.CallToolRequest, in EventsInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectEvents(ctx, in, clamp(in.Limit, defaultLimit, maxLimit))
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	b.WriteString(sel.header)
	if len(sel.events) == 0 {
		b.WriteString("None found.\n")
	} else {
		if sel.full() {
			fmt.Fprintf(&b, "Newest %d (there may be more):\n", sel.limit)
		} else {
			fmt.Fprintf(&b, "%d found, newest first:\n", len(sel.events))
		}
		for _, e := range sel.events {
			b.WriteString(eventLine(e, sel.now))
		}
		if sel.full() {
			b.WriteString(olderHint(sel.events[len(sel.events)-1].Start.Time, sel.now))
		}
		b.WriteString("Use get_event_snapshot with an id to show the picture, or get_events_grid to see several at once.\n")
	}
	slog.Info("get_events", "period", sel.period, "label", in.Label, "camera", in.Camera, "zone", in.Zone, "found", len(sel.events))
	return text(b.String()), nil, nil
}

// eventSelection is the result of an event query or search, shared by the list and grid tools.
type eventSelection struct {
	header string // first line(s) of the answer: what was asked for, and the time zone
	period string
	events []frigate.Event
	limit  int
	now    time.Time
}

// full reports whether the query hit its limit, so there may be more (older) items.
func (s eventSelection) full() bool { return len(s.events) > 0 && len(s.events) >= s.limit }

// olderHint tells the model exactly how to ask for the items before the oldest one shown.
func olderHint(oldest, now time.Time) string {
	return fmt.Sprintf("For older ones, call again with the same arguments and to: %s\n", timeArg(oldest, now))
}

// selectEvents runs the tracked-object query behind get_events and get_events_grid.
func (t *Toolset) selectEvents(ctx context.Context, in EventsInput, limit int) (eventSelection, error) {
	sel := eventSelection{limit: limit}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return sel, unavailable(err)
	}
	sel.now = t.opt.Now().In(t.opt.Location)
	after, before, period, err := timeRange{in.Since, in.From, in.To}.resolve("24h", sel.now)
	if err != nil {
		return sel, err
	}
	sel.period = period
	f := frigate.Filter{After: after, Before: before, Limit: limit}
	if f.Cameras, err = cat.filterCameras(in.Camera, len(t.opt.Allowed) > 0); err != nil {
		return sel, err
	}
	if in.Label != "" {
		l, err := cat.label(in.Label)
		if err != nil {
			return sel, err
		}
		f.Labels = []string{l}
	}
	if in.Zone != "" {
		z, err := cat.zone(in.Zone)
		if err != nil {
			return sel, err
		}
		f.Zones = []string{z}
	}
	evs, err := t.src.Events(ctx, f)
	if err != nil {
		slog.Error("event query", "err", err)
		return sel, unavailable(err)
	}
	sel.events = visibleEvents(evs, cat)

	var b strings.Builder
	what := "Tracked objects"
	if len(f.Labels) > 0 {
		what = "Tracked " + f.Labels[0] + " objects"
	}
	fmt.Fprintf(&b, "%s, %s", what, period)
	if in.Camera != "" {
		fmt.Fprintf(&b, ", %s", f.Cameras[0])
	}
	if len(f.Zones) > 0 {
		fmt.Fprintf(&b, ", zone %s", f.Zones[0])
	}
	fmt.Fprintf(&b, ". Times are %s.\n", sel.now.Format("MST"))
	sel.header = b.String()
	return sel, nil
}

func eventLine(e frigate.Event, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s (%s), %s: %s", clock(e.Start.Time, now), span(e.Start.Time, endTime(e.End)), e.Camera, e.Label)
	if e.SubLabel != "" {
		fmt.Fprintf(&b, " (%s)", e.SubLabel)
	}
	if score := max(e.Data.TopScore, e.Data.Score); score > 0 {
		fmt.Fprintf(&b, " %d%%", int(score*100+0.5))
	}
	if len(e.Zones) > 0 {
		fmt.Fprintf(&b, "; zones: %s", strings.Join(dedupe(e.Zones), ", "))
	}
	if e.Data.Plate != "" {
		fmt.Fprintf(&b, "; plate: %s", e.Data.Plate)
	}
	if !e.HasSnapshot {
		b.WriteString("; no snapshot")
	}
	fmt.Fprintf(&b, "; id %s\n", e.ID)
	if e.Data.Description != "" {
		fmt.Fprintf(&b, "  Description: %s\n", oneLine(e.Data.Description, 300))
	}
	return b.String()
}

func (t *Toolset) eventSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return nil, nil, errors.New("id is required; get it from get_events")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	e, err := t.src.Event(ctx, id)
	if errors.Is(err, frigate.ErrNotFound) || (err == nil && !cat.visible(e.Camera)) {
		return nil, nil, fmt.Errorf("no tracked object with id %q; get ids from get_events", id)
	}
	if err != nil {
		slog.Error("get_event_snapshot", "id", id, "err", err)
		return nil, nil, unavailable(err)
	}
	if !e.HasSnapshot {
		return nil, nil, fmt.Errorf("no snapshot was saved for %s; snapshots may be off for camera %s", id, e.Camera)
	}
	jpeg, err := t.src.EventSnapshot(ctx, id, t.opt.SnapshotHeight)
	if err != nil {
		slog.Error("get_event_snapshot", "id", id, "err", err)
		if errors.Is(err, frigate.ErrNotFound) {
			return nil, nil, fmt.Errorf("the snapshot for %s is no longer available", id)
		}
		return nil, nil, unavailable(err)
	}
	now := t.opt.Now().In(t.opt.Location)
	slog.Info("get_event_snapshot", "id", id, "camera", e.Camera, "bytes", len(jpeg))
	return imageResult(fmt.Sprintf("%s on %s at %s", capitalize(e.Label), e.Camera, clock(e.Start.Time, now)), jpeg), nil, nil
}

func (t *Toolset) searchEvents(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectSearch(ctx, in, clamp(in.Limit, searchLimit, maxSearch))
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	b.WriteString(sel.header)
	if len(sel.events) == 0 {
		b.WriteString("No matches.\n")
	} else {
		for _, e := range sel.events {
			b.WriteString(eventLine(e, sel.now))
		}
		b.WriteString("Matches are by appearance and may be wrong; check with get_event_snapshot, or search_events_grid to see several at once.\n")
	}
	slog.Info("search_events", "period", sel.period, "camera", in.Camera, "found", len(sel.events))
	return text(b.String()), nil, nil
}

// selectSearch runs the semantic search behind search_events and search_events_grid.
func (t *Toolset) selectSearch(ctx context.Context, in SearchInput, limit int) (eventSelection, error) {
	sel := eventSelection{limit: limit}
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return sel, errors.New("query is required, e.g. \"red pickup truck\"")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return sel, unavailable(err)
	}
	sel.now = t.opt.Now().In(t.opt.Location)
	f := frigate.Filter{Limit: limit}
	var period string
	if f.After, f.Before, period, err = (timeRange{in.Since, in.From, in.To}).resolve("", sel.now); err != nil {
		return sel, err
	}
	sel.period = period
	if f.Cameras, err = cat.filterCameras(in.Camera, len(t.opt.Allowed) > 0); err != nil {
		return sel, err
	}
	evs, err := t.src.Search(ctx, query, f)
	if err != nil {
		slog.Error("search", "err", err)
		return sel, unavailable(err)
	}
	sel.events = visibleEvents(evs, cat)
	sel.header = fmt.Sprintf("Best matches for %q, %s. Times are %s.\n", query, period, sel.now.Format("MST"))
	return sel, nil
}

func visibleEvents(evs []frigate.Event, cat *catalog) []frigate.Event {
	out := evs[:0]
	for _, e := range evs {
		if cat.visible(e.Camera) {
			out = append(out, e)
		}
	}
	return out
}

// unavailable hides Frigate's error details (which stay in the log) behind a short message.
func unavailable(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("frigate didn't answer in time (details in the frigate-mcp log)")
	case strings.Contains(err.Error(), "login") || strings.Contains(err.Error(), "authori"):
		return errors.New("frigate-mcp can't log in to Frigate (details in the frigate-mcp log)")
	}
	return errors.New("frigate is unreachable or returned an error (details in the frigate-mcp log)")
}

func errorResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func imageResult(caption string, jpeg []byte) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: caption},
		&mcp.ImageContent{Data: jpeg, MIMEType: "image/jpeg"},
	}}
}

func endTime(t *frigate.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	return &t.Time
}

func clamp(n, def, hi int) int {
	if n <= 0 {
		return def
	}
	return min(n, hi)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
