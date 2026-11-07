package tools

// get_event_frames: a tracked object at a few moments of its visit, from the recordings, in one
// picture. A real visitor walks in from somewhere; a false detection tends to sit in one place.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

// shot is one moment to show: when, why, and where the object was.
type shot struct {
	at    time.Time
	label string
	box   []float64
}

func (t *Toolset) eventFrames(ctx context.Context, _ *mcp.CallToolRequest, in IDInput) (*mcp.CallToolResult, any, error) {
	o, err := t.resolveObjects(ctx, "get_event_frames", in.ID)
	if err != nil {
		return nil, nil, err
	}
	caption, jpg, err := t.framesPicture(ctx, "get_event_frames", in.ID, o)
	if err != nil {
		return nil, nil, err
	}
	return imageResult(caption, jpg), nil, nil
}

// framesPicture is the picture of o's first object at a few moments, and its caption, as
// get_event_frames gives them.
func (t *Toolset) framesPicture(ctx context.Context, tool, id string, o objects) (string, []byte, error) {
	obj := o.ids[0] // an alert's first object
	entries, err := t.src.Timeline(ctx, []string{obj}, 100)
	if err != nil {
		slog.Error(tool, "id", id, "err", err)
		return "", nil, unavailable(err)
	}
	if len(entries) == 0 {
		return "", nil, fmt.Errorf("no timeline for %s in Frigate any more, so the moments to show aren't known; try get_alert_snapshot", id)
	}
	shots := pickShots(entries)
	now := t.opt.Now().In(t.opt.Location)
	l := newLayout(len(shots), t.gridHeight())
	l.corner = t.opt.GridLabelCorner
	pics := make([]image.Image, len(shots))
	labels := make([]string, len(shots))
	var b strings.Builder
	label := entries[0].Data.Label
	if label == "" {
		label = o.what
	}
	fmt.Fprintf(&b, "%s on %s at %d moments, from the recordings, each zoomed in on it (left to right, top to bottom):\n",
		capitalize(label), o.camera, len(shots))
	missing := 0
	for i, s := range shots {
		clockAt := s.at.In(now.Location()).Format("15:04:05")
		labels[i] = fmt.Sprintf("%d) %s %s", i+1, clockAt, s.label)
		fmt.Fprintf(&b, "%d. %s: %s\n", i+1, clockAt, s.label)
		jpg, err := t.src.RecordingFrame(ctx, o.camera, float64(s.at.UnixNano())/1e9, 0)
		if err == nil {
			pics[i], err = jpeg.Decode(bytes.NewReader(jpg))
		}
		if err != nil {
			slog.Warn(tool+": no picture", "moment", i+1, "err", err)
			missing++
			continue
		}
		pics[i] = zoom(pics[i], s.box)
	}
	if missing == len(shots) {
		return "", nil, fmt.Errorf("no recorded frames for %s on %s; recordings may be off or already deleted. Try get_alert_snapshot", id, o.camera)
	}
	if missing > 0 {
		fmt.Fprintf(&b, "%d of the moments have no recorded frame.\n", missing)
	}
	jpg, err := l.render(pics, labels)
	if err != nil {
		return "", nil, err
	}
	slog.Info(tool, "id", id, "object", obj, "moments", len(shots), "missing", missing, "bytes", len(jpg))
	return strings.TrimSpace(b.String()), jpg, nil
}

// pickShots chooses up to four moments: first seen, entering a zone, the middle of the stay,
// and last seen. Moments less than a second apart count once.
func pickShots(entries []frigate.TimelineEntry) []shot {
	boxAt := func(at time.Time) []float64 { // the object's box nearest to at
		var best []float64
		bestGap := time.Duration(math.MaxInt64)
		for _, en := range entries {
			if len(en.Data.Box) != 4 {
				continue
			}
			gap := en.Timestamp.Sub(at)
			if gap < 0 {
				gap = -gap
			}
			if gap < bestGap {
				best, bestGap = en.Data.Box, gap
			}
		}
		return best
	}
	first := entries[0].Timestamp.Time
	last := entries[len(entries)-1]
	end := last.Timestamp.Time
	if last.ClassType == "gone" {
		// Frigate says gone after the object has left the frame: look just before.
		end = end.Add(-2 * time.Second)
		if end.Before(first) {
			end = first
		}
	}
	shots := []shot{{at: first, label: "first seen"}}
	for _, en := range entries {
		if en.ClassType == "entered_zone" {
			shots = append(shots, shot{at: en.Timestamp.Time, label: "entered " + strings.Join(dedupe(en.Data.Zones), ", ")})
			break
		}
	}
	from := shots[len(shots)-1].at
	shots = append(shots, shot{at: from.Add(end.Sub(from) / 2), label: "while there"}, shot{at: end, label: "last seen"})
	var out []shot
	for _, s := range shots {
		if len(out) > 0 && s.at.Sub(out[len(out)-1].at) < time.Second {
			continue
		}
		s.box = boxAt(s.at)
		out = append(out, s)
	}
	return out
}

// zoom cuts img down to the area around box (x, y, width, height as fractions of the frame),
// with room around the object for context and the 16:9 shape of a tile. Without a box, or for
// an object that already fills much of the frame, img is returned as it is.
func zoom(img image.Image, box []float64) image.Image {
	if len(box) != 4 {
		return img
	}
	r := img.Bounds()
	fw, fh := float64(r.Dx()), float64(r.Dy())
	// Three times the object's height, but at least a third of the frame.
	h := math.Max(box[3]*fh*3, fh/3)
	w := h * 16 / 9
	if w > fw || h > fh {
		return img
	}
	cx, cy := (box[0]+box[2]/2)*fw, (box[1]+box[3]/2)*fh
	x0 := math.Min(math.Max(cx-w/2, 0), fw-w)
	y0 := math.Min(math.Max(cy-h/2, 0), fh-h)
	crop := image.Rect(r.Min.X+int(x0), r.Min.Y+int(y0), r.Min.X+int(x0+w), r.Min.Y+int(y0+h))
	if sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(crop)
	}
	return img
}
