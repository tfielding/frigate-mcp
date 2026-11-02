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
}

type Options struct {
	Allowed        map[string]bool // lowercased camera names; empty = all cameras
	SnapshotHeight int             // image height in pixels; 0 = native
	Location       *time.Location  // time zone for times shown to the model
	Now            func() time.Time
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
	Since    string `json:"since,omitempty" jsonschema:"how far back: e.g. 30m, 6h, 2d, 1w, today, yesterday. Default 24h"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum items to list, newest first. Default 10"`
}

type IDInput struct {
	ID string `json:"id" jsonschema:"the id shown in the list"`
}

type EventsInput struct {
	Camera string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Label  string `json:"label,omitempty" jsonschema:"only this kind of object, e.g. person, car, dog"`
	Zone   string `json:"zone,omitempty" jsonschema:"only objects that entered this zone"`
	Since  string `json:"since,omitempty" jsonschema:"how far back: e.g. 30m, 6h, 2d, 1w, today, yesterday. Default 24h"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum objects to list, newest first. Default 10"`
}

type SearchInput struct {
	Query  string `json:"query" jsonschema:"what to look for, in plain words, e.g. red pickup truck or person carrying a box"`
	Camera string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Since  string `json:"since,omitempty" jsonschema:"how far back: e.g. 6h, 2d, 1w. Default: all retained history"`
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
		Name: "list_events",
		Description: "List individual objects Frigate tracked (person, car, dog, ...), newest first, with time, camera, " +
			"zones and an id. Use for questions like \"when was the last car in the driveway?\".",
		Annotations: readOnly("Frigate events"),
	}, t.events)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_event_snapshot",
		Description: "Show the saved snapshot (JPEG) of one tracked object from list_events or search_events, with its box drawn.",
		Annotations: readOnly("Event snapshot"),
	}, t.eventSnapshot)

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
			Description: "Search tracked objects by what they look like, in plain words (\"red pickup truck\", \"person with a dog\"). " +
				"Returns the best matches with time, camera and an id; use get_event_snapshot to show one.",
			Annotations: readOnly("Search events"),
		}, t.searchEvents)
		t.search = true
		slog.Info("semantic search enabled in frigate: search_events available")
	case !cat.semantic && t.search:
		t.server.RemoveTools("search_events")
		t.search = false
		slog.Info("semantic search disabled in frigate: search_events removed")
	}
}

func readOnly(title string) *mcp.ToolAnnotations {
	f := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, DestructiveHint: &f, OpenWorldHint: &f}
}

func (t *Toolset) alerts(ctx context.Context, _ *mcp.CallToolRequest, in AlertsInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectReviews(ctx, in.Severity, in.Camera, in.Since)
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
		b.WriteString("Use get_alert_snapshot with an id to show the picture, or get_alerts_grid to see several at once.\n")
	}
	slog.Info("get_alerts", "severity", sel.severity, "since", sel.period, "alerts", sel.alerts, "detections", sel.detections, "listed", len(shown))
	return text(b.String()), nil, nil
}

// selectReviews runs the review query behind get_alerts and get_alerts_grid: counts of both
// severities over the window, and the items of the requested severity, newest first.
func (t *Toolset) selectReviews(ctx context.Context, severityArg, camera, since string) (reviewSelection, error) {
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
	after, before, period, err := window(since, "24h", sel.now)
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
	jpeg, err := t.reviewPicture(ctx, r, t.opt.SnapshotHeight)
	if err != nil {
		slog.Warn("get_alert_snapshot: no image", "id", id, "err", err)
		return nil, nil, fmt.Errorf("no picture available for %s; recordings and snapshots may be off for camera %s", id, r.Camera)
	}
	now := t.opt.Now().In(t.opt.Location)
	what := strings.Join(dedupe(r.Data.Objects), ", ")
	slog.Info("get_alert_snapshot", "id", id, "camera", r.Camera, "bytes", len(jpeg))
	return imageResult(fmt.Sprintf("%s on %s at %s: %s", capitalize(r.Severity), r.Camera, clock(r.Start.Time, now), what), jpeg), nil, nil
}

// reviewPicture returns the best picture of a review item: the recorded frame Frigate uses as its
// thumbnail or, if recordings are off, the snapshot of one of the objects in it.
func (t *Toolset) reviewPicture(ctx context.Context, r *frigate.Review, height int) ([]byte, error) {
	err := errors.New("no thumbnail time or objects")
	if r.Data.ThumbTime > 0 {
		var jpeg []byte
		if jpeg, err = t.src.RecordingFrame(ctx, r.Camera, r.Data.ThumbTime, height); err == nil {
			return jpeg, nil
		}
	}
	for _, ev := range r.Data.Detections {
		var jpeg []byte
		if jpeg, err = t.src.EventSnapshot(ctx, ev, height); err == nil {
			return jpeg, nil
		}
	}
	return nil, err
}

func (t *Toolset) events(ctx context.Context, _ *mcp.CallToolRequest, in EventsInput) (*mcp.CallToolResult, any, error) {
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	now := t.opt.Now().In(t.opt.Location)
	after, before, period, err := window(in.Since, "24h", now)
	if err != nil {
		return nil, nil, err
	}
	f := frigate.Filter{After: after, Before: before, Limit: clamp(in.Limit, defaultLimit, maxLimit)}
	if f.Cameras, err = cat.filterCameras(in.Camera, len(t.opt.Allowed) > 0); err != nil {
		return nil, nil, err
	}
	if in.Label != "" {
		l, err := cat.label(in.Label)
		if err != nil {
			return nil, nil, err
		}
		f.Labels = []string{l}
	}
	if in.Zone != "" {
		z, err := cat.zone(in.Zone)
		if err != nil {
			return nil, nil, err
		}
		f.Zones = []string{z}
	}
	evs, err := t.src.Events(ctx, f)
	if err != nil {
		slog.Error("list_events", "err", err)
		return nil, nil, unavailable(err)
	}
	evs = visibleEvents(evs, cat)

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
	fmt.Fprintf(&b, ". Times are %s.\n", now.Format("MST"))
	if len(evs) == 0 {
		b.WriteString("None found.\n")
	} else {
		if len(evs) == f.Limit {
			fmt.Fprintf(&b, "Newest %d (there may be more):\n", f.Limit)
		}
		for _, e := range evs {
			b.WriteString(eventLine(e, now))
		}
		b.WriteString("Use get_event_snapshot with an id to show the picture.\n")
	}
	slog.Info("list_events", "since", period, "label", strings.Join(f.Labels, ","), "found", len(evs))
	return text(b.String()), nil, nil
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
		return nil, nil, errors.New("id is required; get it from list_events")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	e, err := t.src.Event(ctx, id)
	if errors.Is(err, frigate.ErrNotFound) || (err == nil && !cat.visible(e.Camera)) {
		return nil, nil, fmt.Errorf("no tracked object with id %q; get ids from list_events", id)
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
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, nil, errors.New("query is required, e.g. \"red pickup truck\"")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	now := t.opt.Now().In(t.opt.Location)
	f := frigate.Filter{Limit: clamp(in.Limit, searchLimit, maxSearch)}
	period := "all retained history"
	if in.Since != "" {
		if f.After, f.Before, period, err = window(in.Since, "", now); err != nil {
			return nil, nil, err
		}
	}
	if f.Cameras, err = cat.filterCameras(in.Camera, len(t.opt.Allowed) > 0); err != nil {
		return nil, nil, err
	}
	evs, err := t.src.Search(ctx, query, f)
	if err != nil {
		slog.Error("search_events", "err", err)
		return nil, nil, unavailable(err)
	}
	evs = visibleEvents(evs, cat)
	var b strings.Builder
	fmt.Fprintf(&b, "Best matches for %q, %s. Times are %s.\n", query, period, now.Format("MST"))
	if len(evs) == 0 {
		b.WriteString("No matches.\n")
	} else {
		for _, e := range evs {
			b.WriteString(eventLine(e, now))
		}
		b.WriteString("Matches are by appearance and may be wrong; check with get_event_snapshot.\n")
	}
	slog.Info("search_events", "found", len(evs))
	return text(b.String()), nil, nil
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
