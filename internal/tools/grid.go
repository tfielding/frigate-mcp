package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

const (
	gridDefault      = 6
	gridMax          = 9
	gridFetches      = 4    // pictures fetched from Frigate at once
	gridNativeHeight = 1080 // grid height when SNAPSHOT_HEIGHT is 0 (native)
	gridMinHeight    = 360
	gridGap          = 4
)

var (
	gridBackground  = color.RGBA{24, 24, 24, 255}
	labelBackground = color.RGBA{0, 0, 0, 200}
	labelText       = color.RGBA{255, 220, 0, 255}
)

// Corner is where tile labels go. The default is bottom-left, because Frigate draws its own
// timestamp top-left by default.
type Corner string

const (
	TopLeft     Corner = "top-left"
	TopRight    Corner = "top-right"
	BottomLeft  Corner = "bottom-left"
	BottomRight Corner = "bottom-right"
	center      Corner = "center"
)

// ParseCorner reads a label position: top-left, top-right, bottom-left or bottom-right (also
// tl, tr, bl, br, with spaces or underscores). Empty means bottom-left.
func ParseCorner(s string) (Corner, error) {
	switch strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(strings.TrimSpace(s))) {
	case "", "bottom-left", "bl":
		return BottomLeft, nil
	case "top-left", "tl":
		return TopLeft, nil
	case "top-right", "tr":
		return TopRight, nil
	case "bottom-right", "br":
		return BottomRight, nil
	}
	return "", fmt.Errorf("label position %q not understood; use top-left, top-right, bottom-left or bottom-right", s)
}

// Crop is when a grid tile zooms in on its object instead of showing the whole frame.
type Crop string

const (
	CropAuto   Crop = "auto"   // when the object is small in the frame
	CropAlways Crop = "always" // always
	CropOff    Crop = "off"    // never
)

// smallObject: an object whose larger side is under this share of the frame is zoomed in on in
// auto mode. Below it, a person at the end of the driveway is a few pixels in a tile.
const smallObject = 0.4

// ParseCrop reads auto, always or off (also true/false). Empty means auto.
func ParseCrop(s string) (Crop, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return CropAuto, nil
	case "always", "on", "true", "yes":
		return CropAlways, nil
	case "off", "never", "false", "no":
		return CropOff, nil
	}
	return "", fmt.Errorf("%q not understood; use auto, always or off", s)
}

// small reports whether an object's box (x, y, width, height as fractions of the frame) is small
// enough to zoom in on. Without a box, it isn't.
func small(box []float64) bool {
	return len(box) == 4 && box[2] > 0 && box[3] > 0 && max(box[2], box[3]) < smallObject
}

type GridInput struct {
	Severity string `json:"severity,omitempty" jsonschema:"alert (default), detection, or all"`
	Camera   string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Since    string `json:"since,omitempty" jsonschema:"how far back: e.g. 30m, 6h, 2d, 1w, today, yesterday. Default 24h. For a time of day use from and to instead"`
	From     string `json:"from,omitempty" jsonschema:"start of a time range, e.g. 11:30, 2pm, noon, yesterday 18:00. For \"around noon\" use from 11:30 and to 12:30"`
	To       string `json:"to,omitempty" jsonschema:"end of a time range, same forms as from. Default now"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many pictures, newest first. Default 6, at most 9"`
}

// tile is one picture in a grid: its line in the text list, its label on the image, and how to
// fetch its picture at a given height.
type tile struct {
	line  string // without the leading number
	label string // without the leading number
	fetch func(ctx context.Context, height int) ([]byte, error)
}

// grid finishes a grid tool's answer: b already holds the header, total is how many items matched.
// It numbers the tiles, fetches their pictures and returns the list with one grid image. It's
// used so a model that handles one image well (or has little context) can see several at once.
// gridAnswer is what a grid tool hands to grid.
type gridAnswer struct {
	tool   string
	period string           // the time range, for the log
	b      *strings.Builder // holds the header already
	tiles  []tile
	total  int    // how many items matched; more than len(tiles) when cut to the limit
	more   string // heading to use instead of the default, e.g. when the query hit its limit
	older  string // how to ask for older items; written when not everything is shown
	footer string
}

func (t *Toolset) grid(ctx context.Context, a gridAnswer) *mcp.CallToolResult {
	tool, b, tiles, total, more, older, footer := a.tool, a.b, a.tiles, a.total, a.more, a.older, a.footer
	if len(tiles) == 0 {
		b.WriteString("Nothing to show.\n")
		slog.Info(tool, "period", a.period, "shown", 0)
		return text(b.String())
	}
	switch {
	case total > len(tiles):
		fmt.Fprintf(b, "Pictures of the newest %d of %d, numbered as in the grid (left to right, top to bottom):\n", len(tiles), total)
	case more != "":
		b.WriteString(more)
	default:
		b.WriteString("Pictures numbered as in the grid (left to right, top to bottom), newest first:\n")
	}

	l := newLayout(len(tiles), t.gridHeight())
	l.corner = t.opt.GridLabelCorner
	pics := t.fetchPictures(ctx, tool, tiles, l.cellH)
	missing := 0
	labels := make([]string, len(tiles))
	for i, tl := range tiles {
		fmt.Fprintf(b, "%d. %s", i+1, tl.line)
		labels[i] = fmt.Sprintf("%d) %s", i+1, tl.label)
		if pics[i] == nil {
			missing++
		}
	}
	if missing == len(tiles) {
		b.WriteString("No pictures are available for these; recordings and snapshots may be off.\n")
		slog.Warn(tool+": no pictures", "items", len(tiles))
		return text(b.String())
	}
	switch {
	case missing == 1:
		b.WriteString("1 item has no picture (shown as an empty tile).\n")
	case missing > 1:
		fmt.Fprintf(b, "%d items have no picture (shown as empty tiles).\n", missing)
	}
	if older != "" && (total > len(tiles) || more != "") {
		b.WriteString(older)
	}
	b.WriteString(footer)

	jpg, err := l.render(pics, labels)
	if err != nil {
		slog.Error(tool+": encode", "err", err)
		return errorResult("couldn't build the picture grid (details in the frigate-mcp log)")
	}
	slog.Info(tool, "period", a.period, "shown", len(tiles), "missing", missing, "bytes", len(jpg))
	return imageResult(b.String(), jpg)
}

// alertsGrid shows the newest alerts as one numbered picture grid.
func (t *Toolset) alertsGrid(ctx context.Context, _ *mcp.CallToolRequest, in GridInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectReviews(ctx, in.Severity, in.Camera, timeRange{in.Since, in.From, in.To})
	if err != nil {
		return nil, nil, err
	}
	shown := sel.shown[:min(len(sel.shown), clamp(in.Limit, gridDefault, gridMax))]
	tiles := make([]tile, len(shown))
	for i := range shown {
		r := &shown[i]
		tiles[i] = tile{
			line:  strings.TrimPrefix(reviewLine(*r, sel.severity == "all", sel.now), "- "),
			label: tileLabel(r.Start.Time, r.Camera, dedupe(r.Data.Objects), sel.now.Location(), t.opt.GridCamera),
			fetch: func(ctx context.Context, height int) ([]byte, error) {
				return t.reviewPicture(ctx, r, height, t.opt.GridBoxes, t.opt.GridCrop)
			},
		}
	}
	var b strings.Builder
	b.WriteString(sel.header())
	older := ""
	if len(shown) > 0 {
		older = olderHint(shown[len(shown)-1].Start.Time, sel.now)
	}
	return t.grid(ctx, gridAnswer{tool: "get_alerts_grid", period: sel.period, b: &b, tiles: tiles, total: len(sel.shown),
		older: older, footer: "Use get_alert_snapshot with an id for a bigger picture of one.\n"}), nil, nil
}

// eventTiles makes grid tiles of tracked objects, using their saved snapshots (with boxes).
func (t *Toolset) eventTiles(evs []frigate.Event, now time.Time) []tile {
	tiles := make([]tile, len(evs))
	for i, e := range evs {
		tiles[i] = tile{
			line:  strings.TrimPrefix(eventLine(e, now), "- "),
			label: tileLabel(e.Start.Time, e.Camera, []string{e.Label}, now.Location(), t.opt.GridCamera),
			fetch: func(ctx context.Context, height int) ([]byte, error) {
				if !e.HasSnapshot {
					return nil, errors.New("no snapshot saved")
				}
				zoom := t.opt.GridCrop == CropAlways || (t.opt.GridCrop == CropAuto && small(e.Data.Box))
				return t.src.EventSnapshot(ctx, e.ID, height, zoom)
			},
		}
	}
	return tiles
}

// eventsGrid shows the newest tracked objects matching the filters as one numbered picture grid.
func (t *Toolset) eventsGrid(ctx context.Context, _ *mcp.CallToolRequest, in EventsInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectEvents(ctx, in, clamp(in.Limit, gridDefault, gridMax))
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	b.WriteString(sel.header)
	more, older := "", ""
	if sel.full() {
		more = fmt.Sprintf("Pictures of the newest %d (there may be more), numbered as in the grid (left to right, top to bottom):\n", sel.limit)
		older = olderHint(sel.events[len(sel.events)-1].Start.Time, sel.now)
	}
	return t.grid(ctx, gridAnswer{tool: "get_events_grid", period: sel.period, b: &b, tiles: t.eventTiles(sel.events, sel.now),
		total: len(sel.events), more: more, older: older, footer: "Use get_event_snapshot with an id for a bigger picture of one.\n"}), nil, nil
}

// searchGrid shows the best semantic search matches as one numbered picture grid.
func (t *Toolset) searchGrid(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectSearch(ctx, in, clamp(in.Limit, gridDefault, gridMax))
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	b.WriteString(sel.header)
	return t.grid(ctx, gridAnswer{tool: "search_events_grid", period: sel.period, b: &b, tiles: t.eventTiles(sel.events, sel.now),
		total: len(sel.events), more: "Pictures of the best matches first, numbered as in the grid (left to right, top to bottom):\n",
		footer: "Matches are by appearance and may be wrong. Use get_event_snapshot with an id for a bigger picture of one.\n"}), nil, nil
}

// tileLabel is the text on a tile after its number: 24-hour time and what was seen, e.g.
// "14:12 (person, car)", with the camera before the objects if asked for. It's kept short; the
// list has the full date.
func tileLabel(start time.Time, camera string, objects []string, loc *time.Location, showCamera bool) string {
	s := start.In(loc).Format("15:04")
	if showCamera {
		s += " " + camera
	}
	if len(objects) > 0 {
		s += " (" + strings.Join(objects, ", ") + ")"
	}
	return s
}

// gridHeight is the height the whole grid fits in: the configured snapshot height, but at least
// gridMinHeight so nine tiles stay recognizable.
func (t *Toolset) gridHeight() int {
	if t.opt.SnapshotHeight > 0 {
		return max(t.opt.SnapshotHeight, gridMinHeight)
	}
	return gridNativeHeight
}

// fetchPictures gets each tile's picture at about the tile height, a few at a time. A failed
// picture is nil.
func (t *Toolset) fetchPictures(ctx context.Context, tool string, tiles []tile, height int) []image.Image {
	pics := make([]image.Image, len(tiles))
	sem := make(chan struct{}, gridFetches)
	var wg sync.WaitGroup
	for i := range tiles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			jpg, err := tiles[i].fetch(ctx, height)
			if err != nil {
				slog.Warn(tool+": no picture", "item", i+1, "err", err)
				return
			}
			img, err := jpeg.Decode(bytes.NewReader(jpg))
			if err != nil {
				slog.Warn(tool+": bad picture", "item", i+1, "err", err)
				return
			}
			pics[i] = img
		}()
	}
	wg.Wait()
	return pics
}

// layout is a grid of 16:9 tiles that fits within maxH pixels tall and maxH*16/9 wide.
type layout struct {
	cols, rows   int
	cellW, cellH int
	corner       Corner // where labels go; empty = bottom-left
}

func newLayout(n, maxH int) layout {
	cols := int(math.Ceil(math.Sqrt(float64(n))))
	rows := (n + cols - 1) / cols
	maxW := maxH * 16 / 9
	cellW := min((maxW-gridGap*(cols-1))/cols, ((maxH-gridGap*(rows-1))/rows)*16/9)
	return layout{cols: cols, rows: rows, cellW: cellW, cellH: cellW * 9 / 16}
}

func (l layout) size() (int, int) {
	return l.cols*l.cellW + gridGap*(l.cols-1), l.rows*l.cellH + gridGap*(l.rows-1)
}

// render draws each picture scaled to fit its tile (centered, keeping its shape), with its label
// in the top-left corner, and encodes the grid as JPEG.
func (l layout) render(pics []image.Image, labels []string) ([]byte, error) {
	w, h := l.size()
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(gridBackground), image.Point{}, draw.Src)
	for i, pic := range pics {
		x0 := (i % l.cols) * (l.cellW + gridGap)
		y0 := (i / l.cols) * (l.cellH + gridGap)
		cell := image.Rect(x0, y0, x0+l.cellW, y0+l.cellH)
		if pic != nil {
			draw.ApproxBiLinear.Scale(canvas, fit(pic.Bounds(), cell), pic, pic.Bounds(), draw.Src, nil)
		} else {
			drawLabel(canvas, cell, "no picture", center)
		}
		drawLabel(canvas, cell, labels[i], l.corner)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, canvas, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// fit returns the largest rectangle with src's shape that fits centered in cell.
func fit(src, cell image.Rectangle) image.Rectangle {
	sw, sh := src.Dx(), src.Dy()
	if sw == 0 || sh == 0 {
		return cell
	}
	w, h := cell.Dx(), cell.Dx()*sh/sw
	if h > cell.Dy() {
		w, h = cell.Dy()*sw/sh, cell.Dy()
	}
	x, y := cell.Min.X+(cell.Dx()-w)/2, cell.Min.Y+(cell.Dy()-h)/2
	return image.Rect(x, y, x+w, y+h)
}

// drawLabel writes s on a dark band in a corner (or the center) of cell, clipped to cell. The
// built-in 7x13 font is enlarged with the tile so a vision model can read it. A label too long for
// the tile is cut off at the end, so its number stays visible.
func drawLabel(dst *image.RGBA, cell image.Rectangle, s string, where Corner) {
	face := basicfont.Face7x13
	pad := 3
	tw := font.MeasureString(face, s).Ceil() + 2*pad
	th := face.Height + 2*pad
	small := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.Draw(small, small.Bounds(), image.NewUniform(labelBackground), image.Point{}, draw.Src)
	d := font.Drawer{Dst: small, Src: image.NewUniform(labelText), Face: face,
		Dot: fixed.P(pad, pad+face.Ascent)}
	d.DrawString(s)

	scale := max(1, cell.Dy()/160)
	for scale > 1 && tw*scale > cell.Dx() {
		scale--
	}
	w, h := tw*scale, th*scale
	left, right := cell.Min.X, max(cell.Min.X, cell.Max.X-w)
	top, bottom := cell.Min.Y, cell.Max.Y-h
	pos := image.Pt(left, bottom)
	switch where {
	case TopLeft:
		pos = image.Pt(left, top)
	case TopRight:
		pos = image.Pt(right, top)
	case BottomRight:
		pos = image.Pt(right, bottom)
	case center:
		pos = image.Pt(max(left, cell.Min.X+(cell.Dx()-w)/2), cell.Min.Y+(cell.Dy()-h)/2)
	}
	clip := dst.SubImage(cell).(*image.RGBA)
	draw.NearestNeighbor.Scale(clip, image.Rect(pos.X, pos.Y, pos.X+w, pos.Y+h), small, small.Bounds(), draw.Over, nil)
}

// reviewSelection is the result of a review query shared by get_alerts and get_alerts_grid.
type reviewSelection struct {
	severity           string
	period, where      string
	alerts, detections int
	shown              []frigate.Review // newest first, matching severity
	now                time.Time
}

func (s reviewSelection) header() string {
	return fmt.Sprintf("%s, %s: %s, %s. Times are %s.\n", capitalize(s.period), s.where,
		plural(s.alerts, "alert"), plural(s.detections, "detection"), s.now.Format("MST"))
}
