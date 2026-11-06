package tools

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
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
	if !isBlue(l.cellW/2, l.cellH/2) {
		t.Error("first tile isn't the picture")
	}
	if isBlue(l.cellW+gridGap+l.cellW/2, l.cellH/4) {
		t.Error("missing picture drawn")
	}
	if isBlue(2, l.cellH-3) || !isBlue(2, 2) {
		t.Error("label not in the first tile's bottom-left corner (only)")
	}
}

func TestLabelCorners(t *testing.T) {
	blue := image.NewUniform(color.RGBA{0, 0, 255, 255})
	pic := image.NewRGBA(image.Rect(0, 0, 640, 360))
	draw.Draw(pic, pic.Bounds(), blue, image.Point{}, draw.Src)
	l := newLayout(1, 720)
	for corner, at := range map[Corner]image.Point{
		TopLeft:     {3, 3},
		TopRight:    {l.cellW - 3, 3},
		BottomLeft:  {3, l.cellH - 3},
		BottomRight: {l.cellW - 3, l.cellH - 3},
	} {
		l.corner = corner
		b, err := l.render([]image.Image{pic}, []string{"1) 14:12 (person)"})
		if err != nil {
			t.Fatal(err)
		}
		img, _ := jpeg.Decode(bytes.NewReader(b))
		labelled := 0
		for _, p := range []image.Point{{3, 3}, {l.cellW - 3, 3}, {3, l.cellH - 3}, {l.cellW - 3, l.cellH - 3}} {
			if _, _, bl, _ := img.At(p.X, p.Y).RGBA(); bl < 0x8000 {
				labelled++
				if p != at {
					t.Errorf("%s: label found at %v", corner, p)
				}
			}
		}
		if labelled != 1 {
			t.Errorf("%s: %d corners labelled", corner, labelled)
		}
	}
	for in, want := range map[string]Corner{"": BottomLeft, "TR": TopRight, "top_left": TopLeft, "bottom right": BottomRight} {
		if got, err := ParseCorner(in); err != nil || got != want {
			t.Errorf("ParseCorner(%q) = %q, %v", in, got, err)
		}
	}
}

func TestSmallObject(t *testing.T) {
	for _, tc := range []struct {
		box  []float64
		want bool
	}{
		{[]float64{0.7, 0.3, 0.04, 0.12}, true}, // a person at the end of the driveway
		{[]float64{0.6, 0.4, 0.1, 0.08}, true},  // a car down the street
		{[]float64{0.2, 0.1, 0.5, 0.8}, false},  // a person at the door
		{[]float64{0.1, 0.5, 0.45, 0.2}, false}, // a wide car close by
		{[]float64{0, 0, 0.39, 0.39}, true},     // just under 40%
		{nil, false}, {[]float64{0.1, 0.1}, false}, {[]float64{0, 0, 0, 0}, false},
	} {
		if got := small(tc.box); got != tc.want {
			t.Errorf("small(%v) = %v, want %v", tc.box, got, tc.want)
		}
	}
	for in, want := range map[string]Crop{"": CropAuto, "AUTO": CropAuto, "always": CropAlways, "true": CropAlways, "off": CropOff, "no": CropOff} {
		if got, err := ParseCrop(in); err != nil || got != want {
			t.Errorf("ParseCrop(%q) = %q, %v", in, got, err)
		}
	}
}

func TestTileLabel(t *testing.T) {
	objects := []string{"person", "car"}
	for in, want := range map[time.Time]string{
		time.Date(2026, 11, 1, 14, 12, 0, 0, time.UTC): "14:12 (person, car)",
		time.Date(2026, 11, 1, 0, 5, 0, 0, time.UTC):   "00:05 (person, car)",
		time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC):  "09:30 (person, car)",
	} {
		if got := tileLabel(in, "front_door", objects, time.UTC, false); got != want {
			t.Errorf("tileLabel(%v) = %q, want %q", in, got, want)
		}
	}
	at := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	if got := tileLabel(at, "front_door", objects, time.UTC, true); got != "09:30 front_door (person, car)" {
		t.Errorf("with camera: %q", got)
	}
	eastern, _ := time.LoadLocation("America/New_York") // EST from 1 Nov 2026
	if got := tileLabel(at, "front_door", nil, eastern, false); got != "04:30" {
		t.Errorf("US Eastern, no objects: %q", got)
	}
}

func TestCameraStatus(t *testing.T) {
	stats := map[string]frigate.CameraStats{
		"ok":      {CameraFPS: 5, ExpectedFPS: 5, ConnectionQuality: "excellent", DetectionEnabled: true},
		"fair":    {CameraFPS: 4, ExpectedFPS: 5, ConnectionQuality: "fair", ReconnectsLastHour: 1, DetectionEnabled: true},
		"poor":    {CameraFPS: 2.5, ExpectedFPS: 5, ConnectionQuality: "poor", ReconnectsLastHour: 4, DetectionEnabled: true},
		"bad":     {CameraFPS: 0.8, ExpectedFPS: 5, ConnectionQuality: "unusable", DetectionEnabled: false},
		"behind":  {CameraFPS: 5, ExpectedFPS: 5, SkippedFPS: 2, ConnectionQuality: "excellent", DetectionEnabled: true},
		"old":     {CameraFPS: 5, DetectionEnabled: true}, // Frigate before 0.17: no connection quality
		"unknown": {},
	}
	problems, ok := cameraStatus([]string{"bad", "behind", "fair", "missing", "ok", "old", "poor"}, stats)
	wantProblems := []string{
		"bad: very poor connection (0.8 of 5 fps); object detection is off",
		"behind: skipping 2 frames/s (the detector can't keep up)",
		"missing: no status from Frigate",
		"poor: poor connection (2.5 of 5 fps); 4 reconnects in the last hour",
	}
	if strings.Join(problems, "\n") != strings.Join(wantProblems, "\n") {
		t.Errorf("problems:\n%s", strings.Join(problems, "\n"))
	}
	if strings.Join(ok, ",") != "fair,ok,old" {
		t.Errorf("ok: %v", ok)
	}
}

func TestStatusFormatting(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"0.18.2", "0.18.1-6ef3a7b", true},
		{"0.18.2", "0.18.2-6ef3a7b", false},
		{"0.18.2", "0.19.0-beta1", false},
		{"0.19.0", "0.18.10", true},
		{"unknown", "0.18.1", false},
		{"disabled", "0.18.1", false},
		{"", "0.18.1", false},
	} {
		if got := newer(tc.latest, tc.current); got != tc.want {
			t.Errorf("newer(%q, %q) = %v", tc.latest, tc.current, got)
		}
	}
	for mib, want := range map[float64]string{500: "500 MB", 2048: "2.0 GB", 3 << 20: "3.0 TB"} {
		if got := size(mib); got != want {
			t.Errorf("size(%v) = %q", mib, got)
		}
	}
	for d, want := range map[time.Duration]string{
		5 * time.Minute:             "5 minutes",
		2*time.Hour + 1*time.Minute: "2 hours 1 minute",
		50 * time.Hour:              "2 days 2 hours",
	} {
		if got := uptime(d); got != want {
			t.Errorf("uptime(%v) = %q", d, got)
		}
	}
	separate := storageStatus(map[string]frigate.StorageStats{
		"/media/frigate/recordings": {Total: 1 << 20, Used: 1<<20 - 300<<10, Free: 300 << 10},
		"/media/frigate/clips":      {Total: 100 << 10, Used: 10 << 10, Free: 90 << 10},
		"/media/frigate/exports":    {Total: 1, Used: 1},
	})
	if want := []string{
		"Storage for recordings: 300.0 GB free of 1.0 TB (71% used).",
		"Storage for snapshots and clips: 90.0 GB free of 100.0 GB (10% used).",
	}; strings.Join(separate, "|") != strings.Join(want, "|") {
		t.Errorf("storage: %q", separate)
	}
}

func TestParseTimeArg(t *testing.T) {
	// now is Sun 1 Nov 2026 15:00 UTC.
	at := func(day, h, m, s int) time.Time { return time.Date(2026, 11, day, h, m, s, 0, time.UTC) }
	for in, want := range map[string]time.Time{
		"11:30":                  at(1, 11, 30, 0),
		"9:05":                   at(1, 9, 5, 0),
		"noon":                   at(1, 12, 0, 0),
		"Midnight":               at(1, 0, 0, 0),
		"2pm":                    at(1, 14, 0, 0),
		"2 PM":                   at(1, 14, 0, 0),
		"2:15 p.m.":              at(1, 14, 15, 0),
		"12am":                   at(1, 0, 0, 0),
		"12:30pm":                at(1, 12, 30, 0),
		"14:05:30":               at(1, 14, 5, 30),
		"16:00":                  at(0, 16, 0, 0), /* later than now: yesterday */
		"today 16:00":            at(1, 16, 0, 0),
		"yesterday 18:00":        at(0, 18, 0, 0),
		"yesterday at 6pm":       at(0, 18, 0, 0),
		"yesterday":              at(0, 0, 0, 0),
		"2026-10-30 09:15":       at(30, 9, 15, 0).AddDate(0, -1, 0),
		"2026-10-30 at 12:28:40": at(30, 12, 28, 40).AddDate(0, -1, 0),
	} {
		got, err := parseTimeArg(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTimeArg(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "lunch", "25:00", "13pm", "0am", "11", "11:5", "11:60", "2026-13-01 10:00", "1:2:3:4"} {
		if got, err := parseTimeArg(bad, now); err == nil {
			t.Errorf("parseTimeArg(%q) = %v, want error", bad, got)
		}
	}
	// What timeArg writes, parseTimeArg reads back exactly.
	for _, tm := range []time.Time{now.Add(-150 * time.Minute), now.Add(-20 * time.Hour), now.Add(-80 * time.Hour)} {
		tm = tm.Add(17 * time.Second)
		if got, err := parseTimeArg(timeArg(tm, now), now); err != nil || !got.Equal(tm) {
			t.Errorf("round trip of %v via %q: %v, %v", tm, timeArg(tm, now), got, err)
		}
	}
}

func TestTimeRangeAskedEarly(t *testing.T) {
	// Asked at 12:10: "around noon" and "this morning" must not end up yesterday.
	early := time.Date(2026, 11, 1, 12, 10, 0, 0, time.UTC)
	at := func(h, m int) time.Time { return time.Date(2026, 11, 1, h, m, 0, 0, time.UTC) }
	for _, tc := range []struct {
		r             timeRange
		after, before time.Time
	}{
		{timeRange{from: "11:30", to: "12:30"}, at(11, 30), early},
		{timeRange{from: "06:00", to: "noon"}, at(6, 0), at(12, 0)},
		{timeRange{from: "yesterday 20:00", to: "06:00"}, at(20, 0).AddDate(0, 0, -1), at(6, 0)},
	} {
		after, before, _, err := tc.r.resolve("24h", early)
		if err != nil || !after.Equal(tc.after) || !before.Equal(tc.before) {
			t.Errorf("%+v: got %v..%v %v", tc.r, after, before, err)
		}
	}
	// Asked at 03:00: "last night" ends now, not yesterday morning.
	night := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)
	after, before, _, err := timeRange{from: "yesterday 20:00", to: "06:00"}.resolve("24h", night)
	if err != nil || !after.Equal(at(20, 0).AddDate(0, 0, -1)) || !before.Equal(night) {
		t.Errorf("last night at 03:00: %v..%v %v", after, before, err)
	}
}

func TestTimeRange(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 11, 1, h, m, 0, 0, time.UTC) }
	midnight := at(0, 0)
	for _, tc := range []struct {
		r             timeRange
		def           string
		after, before time.Time
		label         string
	}{
		{timeRange{}, "24h", now.Add(-24 * time.Hour), now, "last 24 hours"},
		{timeRange{}, "", time.Time{}, time.Time{}, "all retained history"},
		{timeRange{from: "11:30", to: "12:30"}, "24h", at(11, 30), at(12, 30), "between today 11:30 and today 12:30"},
		{timeRange{from: "noon"}, "24h", at(12, 0), now, "since today 12:00"},
		{timeRange{to: "12:28"}, "24h", at(12, 28).Add(-24 * time.Hour), at(12, 28), "between yesterday 12:28 and today 12:28"},
		{timeRange{since: "today", to: "12:28"}, "24h", midnight, at(12, 28), "between today 00:00 and today 12:28"},
		{timeRange{since: "2h", to: "noon"}, "24h", at(10, 0), at(12, 0), "between today 10:00 and today 12:00"},
		{timeRange{to: "noon"}, "", time.Time{}, at(12, 0), "up to today 12:00"},
		{timeRange{from: "11:00", to: "today 23:00"}, "24h", at(11, 0), now, "since today 11:00"}, // to in the future: now
		{timeRange{from: "2026-09-01 00:00", to: "today noon"}, "24h", at(12, 0).Add(-maxRange), at(12, 0), "between Thu 1 Oct 12:00 and today 12:00"},
		// A to without a day is the first such time after from, never later than now (15:00).
		{timeRange{from: "14:30", to: "15:30"}, "24h", at(14, 30), now, "since today 14:30"},
		{timeRange{from: "06:00", to: "12:00"}, "24h", at(6, 0), at(12, 0), "between today 06:00 and today 12:00"},
		{timeRange{from: "yesterday 20:00", to: "06:00"}, "24h", at(20, 0).AddDate(0, 0, -1), at(6, 0), "between yesterday 20:00 and today 06:00"},
		{timeRange{from: "13:00", to: "12:00"}, "24h", at(13, 0), now, "since today 13:00"}, // overnight, capped at now
	} {
		after, before, label, err := tc.r.resolve(tc.def, now)
		if err != nil || !after.Equal(tc.after) || !before.Equal(tc.before) || label != tc.label {
			t.Errorf("%+v: got %v..%v %q %v", tc.r, after, before, label, err)
		}
	}
	for _, tc := range []struct {
		r    timeRange
		want string
	}{
		{timeRange{from: "today 13:00", to: "today 12:00"}, "from (today 13:00) must be before to (today 12:00)"},
		{timeRange{from: "lunch"}, `from "lunch" not understood; use e.g. 11:30`},
		{timeRange{to: "later"}, `to "later" not understood`},
		{timeRange{from: "today 18:00"}, "from today 18:00 is in the future"},
	} {
		if _, _, _, err := tc.r.resolve("24h", now); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: got %v, want %q", tc.r, err, tc.want)
		}
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 5 * time.Minute: "5 minutes ago", time.Minute: "1 minute ago",
		3 * time.Hour: "3 hours ago", 47 * time.Hour: "47 hours ago", 72 * time.Hour: "3 days ago",
	} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}
