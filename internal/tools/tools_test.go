package tools

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"golang.org/x/image/draw"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

var now = time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)

func TestWindow(t *testing.T) {
	midnight := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in            string
		after, before time.Time
		label         string
	}{
		{"", now.Add(-24 * time.Hour), now, "last 24 hours"},
		{"30m", now.Add(-30 * time.Minute), now, "last 30 minutes"},
		{"6 hours", now.Add(-6 * time.Hour), now, "last 6 hours"},
		{"last 2d", now.Add(-48 * time.Hour), now, "last 2 days"},
		{"1w", now.Add(-7 * 24 * time.Hour), now, "last 7 days"},
		{"90d", now.Add(-maxRange), now, "last 31 days"},
		{"Today", midnight, now, "today"},
		{"yesterday", midnight.AddDate(0, 0, -1), midnight, "yesterday"},
	} {
		after, before, label, err := window(tc.in, "24h", now)
		if err != nil || !after.Equal(tc.after) || !before.Equal(tc.before) || label != tc.label {
			t.Errorf("%q: got %v..%v %q %v", tc.in, after, before, label, err)
		}
	}
	for _, bad := range []string{"soon", "-2h", "0"} {
		if _, _, _, err := window(bad, "24h", now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestClockAndSpan(t *testing.T) {
	for in, want := range map[time.Time]string{
		now.Add(-time.Hour):      "today 14:00",
		now.Add(-20 * time.Hour): "yesterday 19:00",
		now.Add(-72 * time.Hour): "Thu 29 Oct 15:00",
	} {
		if got := clock(in, now); got != want {
			t.Errorf("clock(%v) = %q, want %q", in, got, want)
		}
	}
	end := now.Add(75 * time.Second)
	if got := span(now, &end); got != "1m15s" {
		t.Errorf("span = %q", got)
	}
	if got := span(now, nil); got != "ongoing" {
		t.Errorf("span = %q", got)
	}
}

func TestCatalogMatching(t *testing.T) {
	on, off := true, false
	cfg := &frigate.Config{Cameras: map[string]frigate.CameraConfig{
		"front_door": {Enabled: &on},
		"Back-Yard":  {},
		"old":        {Enabled: &off},
	}}
	fd := cfg.Cameras["front_door"]
	fd.Objects.Track = []string{"person", "car", "bus"}
	cfg.Cameras["front_door"] = fd
	cat := buildCatalog(cfg, nil)
	for in, want := range map[string]string{"Front Door": "front_door", "back yard": "Back-Yard", "BACK_YARD": "Back-Yard"} {
		if got, err := cat.camera(in); err != nil || got != want {
			t.Errorf("camera(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := cat.camera("old"); err == nil {
		t.Error("disabled camera accepted")
	}
	for in, want := range map[string]string{"people": "person", "Cars": "car", "bus": "bus", "person": "person"} {
		if got, err := cat.label(in); err != nil || got != want {
			t.Errorf("label(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := cat.zone("porch"); err == nil || err.Error() != `unknown zone "porch"; no zones are configured` {
		t.Errorf("zone: %v", err)
	}
}

func TestGridLayout(t *testing.T) {
	for _, tc := range []struct{ n, maxH, cols, rows, w, h int }{
		{1, 720, 1, 1, 1280, 720},
		{2, 720, 2, 1, 1280, 358},
		{4, 720, 2, 2, 1276, 718},
		{6, 720, 3, 2, 1280, 480},
		{9, 720, 3, 3, 1271, 716},
		{9, 1080, 3, 3, 1910, 1076},
	} {
		l := newLayout(tc.n, tc.maxH)
		w, h := l.size()
		if l.cols != tc.cols || l.rows != tc.rows || w != tc.w || h != tc.h {
			t.Errorf("%d tiles in %d: %dx%d grid, %dx%d px", tc.n, tc.maxH, l.cols, l.rows, w, h)
		}
		if w > tc.maxH*16/9 || h > tc.maxH {
			t.Errorf("%d tiles: %dx%d doesn't fit", tc.n, w, h)
		}
	}
}

func TestFit(t *testing.T) {
	cell := image.Rect(100, 0, 740, 360) // 640x360
	for _, tc := range []struct {
		src  image.Rectangle
		want image.Rectangle
	}{
		{image.Rect(0, 0, 1920, 1080), image.Rect(100, 0, 740, 360)}, // same shape: fills it
		{image.Rect(0, 0, 1080, 1920), image.Rect(319, 0, 521, 360)}, // portrait doorbell: centered
		{image.Rect(0, 0, 3200, 900), image.Rect(100, 90, 740, 270)}, // panoramic: letterboxed
	} {
		if got := fit(tc.src, cell); got != tc.want {
			t.Errorf("fit(%v) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

func TestRenderDrawsPicturesAndLabels(t *testing.T) {
	blue := image.NewUniform(color.RGBA{0, 0, 255, 255})
	pic := image.NewRGBA(image.Rect(0, 0, 320, 180))
	draw.Draw(pic, pic.Bounds(), blue, image.Point{}, draw.Src)
	l := newLayout(2, 720)
	b, err := l.render([]image.Image{pic, nil}, []string{"1  today 14:12  front_door", "2  today 02:14  driveway"})
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	isBlue := func(x, y int) bool {
		r, g, bl, _ := img.At(x, y).RGBA()
		return bl > 0xc000 && r < 0x4000 && g < 0x4000
	}
	if !isBlue(l.cellW/2, l.cellH-10) {
		t.Error("first tile isn't the picture")
	}
	if isBlue(l.cellW+gridGap+l.cellW/2, l.cellH-10) {
		t.Error("missing picture drawn")
	}
	if isBlue(2, 2) {
		t.Error("no label in the first tile's corner")
	}
}

func TestTileLabel(t *testing.T) {
	r := frigate.Review{Camera: "front_door"}
	r.Data.Objects = []string{"person", "car", "person"}
	for in, want := range map[time.Time]string{
		time.Date(2026, 11, 1, 14, 12, 0, 0, time.UTC): "3) 14:12 (person, car)",
		time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC):   "3) 00:05 (person, car)",
		time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC):  "3) 09:30 (person, car)",
	} {
		r.Start = frigate.Time{Time: in}
		if got := tileLabel(3, r, time.UTC, false); got != want {
			t.Errorf("tileLabel(%v) = %q, want %q", in, got, want)
		}
	}
	r.Start = frigate.Time{Time: time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)}
	if got := tileLabel(1, r, time.UTC, true); got != "1) 09:30 front_door (person, car)" {
		t.Errorf("with camera: %q", got)
	}
	toronto, _ := time.LoadLocation("America/Toronto") // EST from 1 Nov 2026
	r.Data.Objects = nil
	if got := tileLabel(1, r, toronto, false); got != "1) 04:30" {
		t.Errorf("in Toronto, no objects: %q", got)
	}
}
