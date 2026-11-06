package tools

// get_camera_frame (a camera now or at a past time, or two times side by side) and last_seen.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

type FrameInput struct {
	Camera  string `json:"camera" jsonschema:"camera name"`
	At      string `json:"at,omitempty" jsonschema:"when: now (default), or a time like 15:00, 3pm, yesterday 18:00 or 2026-10-30 09:15"`
	Compare string `json:"compare,omitempty" jsonschema:"optional second time to show side by side with at, e.g. now"`
}

type LastSeenInput struct {
	Label  string `json:"label,omitempty" jsonschema:"kind of object, e.g. car, person, dog"`
	Name   string `json:"name,omitempty" jsonschema:"a recognized person or plate, if Frigate's face or plate recognition knows it"`
	Camera string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Zone   string `json:"zone,omitempty" jsonschema:"only in this zone"`
	Within string `json:"within,omitempty" jsonschema:"how far back to look, e.g. 1d or 1w. Default 31 days, the most"`
}

// moment is a point in time to show: now (live), or a time in the recordings.
type moment struct {
	live bool
	at   time.Time
}

func (t *Toolset) parseMoment(s string, now time.Time) (moment, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "now", "live", "right now", "currently":
		return moment{live: true, at: now}, nil
	}
	at, err := parseTimeArg(s, now)
	if err != nil {
		return moment{}, fmt.Errorf("%q not understood; use now, or a time like 15:00, 3pm, yesterday 18:00 or 2026-10-30 09:15", s)
	}
	if at.After(now) {
		return moment{}, fmt.Errorf("%s is in the future", clock(at, now))
	}
	return moment{at: at}, nil
}

func (m moment) describe(now time.Time) string {
	if m.live {
		return "now (" + clock(m.at, now) + ")"
	}
	return "at " + clock(m.at, now)
}

// frame fetches the camera at m. note says something the model should know about the picture.
func (t *Toolset) frame(ctx context.Context, camera string, m moment, height int, now time.Time) (jpg []byte, note string, err error) {
	if m.live {
		if jpg, err = t.src.LiveFrame(ctx, camera, height); err != nil {
			slog.Error("get_camera_frame", "camera", camera, "err", err)
			return nil, "", unavailable(err)
		}
		// An offline camera's "latest" is an older frame or a placeholder, which Frigate doesn't flag.
		if st, err := t.src.Stats(ctx); err == nil {
			if s, ok := st.Cameras[camera]; ok && s.CameraFPS < 0.1 {
				note = camera + " seems to be offline (no video right now), so this may be an older picture or a placeholder."
			}
		}
		return jpg, note, nil
	}
	jpg, err = t.src.RecordingFrame(ctx, camera, float64(m.at.UnixNano())/1e9, height)
	if errors.Is(err, frigate.ErrNotFound) {
		return nil, "", fmt.Errorf("no recording of %s at %s; Frigate may only keep recordings around events, or not that far back",
			camera, clock(m.at, now))
	}
	if err != nil {
		slog.Error("get_camera_frame", "camera", camera, "err", err)
		return nil, "", unavailable(err)
	}
	return jpg, "", nil
}

func (t *Toolset) cameraFrame(ctx context.Context, _ *mcp.CallToolRequest, in FrameInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Camera) == "" {
		return nil, nil, errors.New("camera is required")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	camera, err := cat.camera(in.Camera)
	if err != nil {
		return nil, nil, err
	}
	now := t.opt.Now().In(t.opt.Location)
	first, err := t.parseMoment(in.At, now)
	if err != nil {
		return nil, nil, fmt.Errorf("at %w", err)
	}
	if strings.TrimSpace(in.Compare) == "" {
		jpg, note, err := t.frame(ctx, camera, first, t.opt.SnapshotHeight, now)
		if err != nil {
			return nil, nil, err
		}
		caption := camera + " " + first.describe(now)
		if note != "" {
			caption += ". " + note
		}
		slog.Info("get_camera_frame", "camera", camera, "live", first.live, "bytes", len(jpg))
		return imageResult(caption, jpg), nil, nil
	}

	second, err := t.parseMoment(in.Compare, now)
	if err != nil {
		return nil, nil, fmt.Errorf("compare %w", err)
	}
	l := newLayout(2, t.gridHeight())
	l.corner = t.opt.GridLabelCorner
	moments := []moment{first, second}
	pics := make([]image.Image, 2)
	labels := make([]string, 2)
	var b strings.Builder
	fmt.Fprintf(&b, "%s, side by side (left to right):\n", camera)
	missing := 0
	for i, m := range moments {
		labels[i] = fmt.Sprintf("%d) %s", i+1, m.at.Format("15:04"))
		if m.live {
			labels[i] = fmt.Sprintf("%d) now %s", i+1, m.at.Format("15:04"))
		}
		jpg, note, err := t.frame(ctx, camera, m, l.cellH, now)
		if err == nil {
			pics[i], err = jpeg.Decode(bytes.NewReader(jpg))
		}
		if err != nil {
			missing++
			fmt.Fprintf(&b, "%d. %s: no picture: %v\n", i+1, m.describe(now), err)
			continue
		}
		fmt.Fprintf(&b, "%d. %s\n", i+1, m.describe(now))
		if note != "" {
			fmt.Fprintf(&b, "   %s\n", note)
		}
	}
	if missing == 2 {
		return nil, nil, errors.New(strings.TrimSpace(b.String()))
	}
	jpg, err := l.render(pics, labels)
	if err != nil {
		slog.Error("get_camera_frame: encode", "err", err)
		return nil, nil, errors.New("couldn't build the picture (details in the frigate-mcp log)")
	}
	slog.Info("get_camera_frame", "camera", camera, "compare", true, "missing", missing, "bytes", len(jpg))
	return imageResult(b.String(), jpg), nil, nil
}

func (t *Toolset) lastSeen(ctx context.Context, _ *mcp.CallToolRequest, in LastSeenInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Label) == "" && strings.TrimSpace(in.Name) == "" {
		return nil, nil, errors.New("give a label (e.g. car, person) or a recognized name")
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	now := t.opt.Now().In(t.opt.Location)
	after, before, period, err := window(in.Within, "31d", now)
	if err != nil {
		return nil, nil, fmt.Errorf("within: %w", err)
	}
	f := frigate.Filter{After: after, Before: before, Limit: 1}
	if f.Cameras, err = cat.filterCameras(in.Camera, len(t.opt.Allowed) > 0); err != nil {
		return nil, nil, err
	}
	what := ""
	if in.Label != "" {
		l, err := cat.label(in.Label)
		if err != nil {
			return nil, nil, err
		}
		f.Labels, what = []string{l}, l
	}
	if in.Name != "" {
		names, err := t.src.SubLabels(ctx)
		if err != nil {
			slog.Error("last_seen", "err", err)
			return nil, nil, unavailable(err)
		}
		name, ok := match(names, in.Name)
		if !ok {
			return nil, nil, fmt.Errorf("unknown name %q; names Frigate has recognized: %s", in.Name, list(names))
		}
		f.SubLabels = []string{name}
		if what == "" {
			what = name
		} else {
			what = name + " (" + what + ")"
		}
	}
	where := ""
	if in.Camera != "" {
		where = " on " + f.Cameras[0]
	}
	if in.Zone != "" {
		z, err := cat.zone(in.Zone)
		if err != nil {
			return nil, nil, err
		}
		f.Zones = []string{z}
		where += " in zone " + z
	}
	evs, err := t.src.Events(ctx, f)
	if err != nil {
		slog.Error("last_seen", "err", err)
		return nil, nil, unavailable(err)
	}
	evs = visibleEvents(evs, cat)
	slog.Info("last_seen", "label", strings.Join(f.Labels, ","), "named", len(f.SubLabels) > 0, "found", len(evs))
	if len(evs) == 0 {
		return text(fmt.Sprintf("No %s seen%s in the %s.\n", what, where, period)), nil, nil
	}
	e := evs[0]
	onCamera := " on " + e.Camera // unless the question already named the camera
	if in.Camera != "" {
		onCamera = ""
	}
	var b strings.Builder
	if end := endTime(e.End); end == nil {
		fmt.Fprintf(&b, "%s%s is there now%s, since %s (%s).", capitalize(what), where, onCamera, clock(e.Start.Time, now), ago(now.Sub(e.Start.Time)))
	} else {
		fmt.Fprintf(&b, "Last %s%s: %s%s (%s), for %s.", what, where, clock(e.Start.Time, now), onCamera,
			ago(now.Sub(e.Start.Time)), span(e.Start.Time, end))
	}
	if len(e.Zones) > 0 {
		fmt.Fprintf(&b, " Zones: %s.", strings.Join(dedupe(e.Zones), ", "))
	}
	if e.SubLabel != "" && len(f.SubLabels) == 0 {
		fmt.Fprintf(&b, " Recognized as %s.", e.SubLabel)
	}
	if e.Data.Plate != "" {
		fmt.Fprintf(&b, " Plate %s.", e.Data.Plate)
	}
	fmt.Fprintf(&b, " Id %s.\n", e.ID)
	if e.HasSnapshot {
		b.WriteString("Use get_event_snapshot with the id to see it.\n")
	}
	return text(b.String()), nil, nil
}

// ago says how long ago in words: "just now", "5 minutes ago", "2 hours ago", "3 days ago".
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return plural(int(d/time.Hour), "hour") + " ago"
	}
	return plural(int(d/(24*time.Hour)), "day") + " ago"
}
