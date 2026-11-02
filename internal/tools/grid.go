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

type GridInput struct {
	Severity string `json:"severity,omitempty" jsonschema:"alert (default), detection, or all"`
	Camera   string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Since    string `json:"since,omitempty" jsonschema:"how far back: e.g. 30m, 6h, 2d, 1w, today, yesterday. Default 24h"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many pictures, newest first. Default 6, at most 9"`
}

// alertsGrid shows the newest alerts as one numbered contact sheet, so a model that handles one
// image well (or has little context) can see several alerts at once.
func (t *Toolset) alertsGrid(ctx context.Context, _ *mcp.CallToolRequest, in GridInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectReviews(ctx, in.Severity, in.Camera, in.Since)
	if err != nil {
		return nil, nil, err
	}
	limit := clamp(in.Limit, gridDefault, gridMax)
	var b strings.Builder
	b.WriteString(sel.header())
	shown := sel.shown
	if len(shown) == 0 {
		b.WriteString("Nothing to show.\n")
		slog.Info("get_alerts_grid", "since", sel.period, "shown", 0)
		return text(b.String()), nil, nil
	}
	if len(shown) > limit {
		fmt.Fprintf(&b, "Pictures of the newest %d of %d, numbered as in the grid (left to right, top to bottom):\n", limit, len(shown))
		shown = shown[:limit]
	} else {
		b.WriteString("Pictures numbered as in the grid (left to right, top to bottom), newest first:\n")
	}

	l := newLayout(len(shown), t.gridHeight())
	pics := t.fetchPictures(ctx, shown, l.cellH)
	missing := 0
	for i, r := range shown {
		line := strings.TrimPrefix(reviewLine(r, sel.severity == "all", sel.now), "- ")
		fmt.Fprintf(&b, "%d. %s", i+1, line)
		if pics[i] == nil {
			missing++
		}
	}
	if missing == len(shown) {
		b.WriteString("No pictures are available for these; recordings and snapshots may be off.\n")
		slog.Warn("get_alerts_grid: no pictures", "items", len(shown))
		return text(b.String()), nil, nil
	}
	switch {
	case missing == 1:
		b.WriteString("1 item has no picture (shown as an empty tile).\n")
	case missing > 1:
		fmt.Fprintf(&b, "%d items have no picture (shown as empty tiles).\n", missing)
	}
	b.WriteString("Use get_alert_snapshot with an id for a bigger picture of one.\n")

	labels := make([]string, len(shown))
	for i, r := range shown {
		labels[i] = tileLabel(i+1, r, sel.now.Location(), t.opt.GridCamera)
	}
	jpg, err := l.render(pics, labels)
	if err != nil {
		slog.Error("get_alerts_grid: encode", "err", err)
		return nil, nil, errors.New("couldn't build the picture grid (details in the frigate-mcp log)")
	}
	slog.Info("get_alerts_grid", "since", sel.period, "shown", len(shown), "missing", missing, "bytes", len(jpg))
	return imageResult(b.String(), jpg), nil, nil
}

// tileLabel is the text on a tile: number, 24-hour time and what was seen, e.g. "3) 14:12 (person, car)",
// with the camera before the objects if asked for. It's kept short; the list has the full date.
func tileLabel(n int, r frigate.Review, loc *time.Location, camera bool) string {
	s := fmt.Sprintf("%d) %s", n, r.Start.In(loc).Format("15:04"))
	if camera {
		s += " " + r.Camera
	}
	if what := dedupe(r.Data.Objects); len(what) > 0 {
		s += " (" + strings.Join(what, ", ") + ")"
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

// fetchPictures gets each item's picture at about the tile height, a few at a time. A failed
// picture is nil.
func (t *Toolset) fetchPictures(ctx context.Context, items []frigate.Review, height int) []image.Image {
	pics := make([]image.Image, len(items))
	sem := make(chan struct{}, gridFetches)
	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			jpg, err := t.reviewPicture(ctx, &items[i], height, t.opt.GridBoxes)
			if err != nil {
				slog.Warn("get_alerts_grid: no picture", "id", items[i].ID, "err", err)
				return
			}
			img, err := jpeg.Decode(bytes.NewReader(jpg))
			if err != nil {
				slog.Warn("get_alerts_grid: bad picture", "id", items[i].ID, "err", err)
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
			drawLabel(canvas, cell, "no picture", true)
		}
		drawLabel(canvas, cell, labels[i], false)
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

// drawLabel writes s on a dark band in cell's top-left corner (or its center), clipped to cell.
// The built-in 7x13 font is enlarged with the tile so a vision model can read it.
func drawLabel(dst *image.RGBA, cell image.Rectangle, s string, center bool) {
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
	pos := cell.Min
	if center {
		pos = pos.Add(image.Pt((cell.Dx()-tw*scale)/2, (cell.Dy()-th*scale)/2))
	}
	clip := dst.SubImage(cell).(*image.RGBA) // a label too long for the tile is cut off
	draw.NearestNeighbor.Scale(clip, image.Rect(pos.X, pos.Y, pos.X+tw*scale, pos.Y+th*scale), small, small.Bounds(), draw.Over, nil)
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
