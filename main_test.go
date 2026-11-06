package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
	"github.com/tfielding/frigate-mcp/internal/tools"
)

var (
	fakeJPEG = solidJPEG(160, 90, color.RGBA{0, 0, 255, 255})
	now      = time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)
)

func solidJPEG(w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func ts(hoursAgo float64) float64 {
	return float64(now.Add(-time.Duration(hoursAgo*float64(time.Hour))).UnixNano()) / 1e9
}

// fakeFrigate mimics the parts of the Frigate 0.18 API we use, behind its login.
type fakeFrigate struct {
	mu       sync.Mutex
	token    string // current valid session token
	logins   int
	requests []string // path?query of API requests
	semantic bool
	noRecs   bool // recordings off: recording snapshots 404
	farAway  bool // e-a1 (the object in every alert) is small in the frame
}

func (f *fakeFrigate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/login" {
		var body struct{ User, Password string }
		json.NewDecoder(r.Body).Decode(&body)
		if body.User != "viewer" || body.Password != "pw" {
			http.Error(w, `{"message":"Login failed"}`, http.StatusUnauthorized)
			return
		}
		f.logins++
		f.token = fmt.Sprintf("hdr.claims%d.sig", f.logins)
		http.SetCookie(w, &http.Cookie{Name: "frigate_token", Value: f.token, HttpOnly: true})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token || f.token == "" {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
	p := r.URL.Path
	switch {
	case p == "/api/config":
		fmt.Fprintf(w, `{
		  "mqtt": {"password": "*******"},
		  "semantic_search": {"enabled": %t},
		  "cameras": {
		    "front_door": {"enabled": true, "objects": {"track": ["person","car"]}, "zones": {"porch": {}, "street": {}},
		                   "audio": {"enabled": true, "listen": ["speech"]}},
		    "driveway":   {"enabled": true, "objects": {"track": ["person","car","dog"]}, "zones": {"driveway": {}}},
		    "bedroom":    {"enabled": true, "objects": {"track": ["person"]}},
		    "old_cam":    {"enabled": false, "objects": {"track": ["bird"]}}
		  }}`, f.semantic)
	case p == "/api/stats":
		fmt.Fprint(w, `{
		  "cameras": {
		    "front_door": {"camera_fps": 5.0, "skipped_fps": 0, "detection_enabled": true, "connection_quality": "excellent",
		                   "expected_fps": 5, "reconnects_last_hour": 0, "stalls_last_hour": 0, "ffmpeg_pid": 41},
		    "driveway":   {"camera_fps": 0.0, "skipped_fps": 0, "detection_enabled": true, "connection_quality": "unusable",
		                   "expected_fps": 5, "reconnects_last_hour": 12, "stalls_last_hour": 0},
		    "bedroom":    {"camera_fps": 5.0, "skipped_fps": 0, "detection_enabled": false, "connection_quality": "excellent",
		                   "expected_fps": 5, "reconnects_last_hour": 0, "stalls_last_hour": 0}
		  },
		  "detectors": {"coral": {"inference_speed": 8.52, "detection_start": 0, "pid": 7}},
		  "detection_fps": 3.1,
		  "service": {"uptime": 273600, "version": "0.18.1-6ef3a7b", "latest_version": "0.18.2",
		    "storage": {
		      "/media/frigate/recordings": {"total": 3815447.8, "used": 3624675.4, "free": 190772.4, "mount_type": "ext4"},
		      "/media/frigate/clips": {"total": 3815447.8, "used": 3624675.4, "free": 190772.4, "mount_type": "ext4"},
		      "/tmp/cache": {"total": 1024.0, "used": 10.0, "free": 1014.0, "mount_type": "tmpfs"},
		      "/dev/shm": {"total": 512.0, "used": 20.0, "free": 492.0, "mount_type": "tmpfs", "min_shm": 64.0}
		    }}
		}`)
	case p == "/api/review":
		w.Write(mustJSON(after(r, []map[string]any{
			review("r-detect", "driveway", "detection", ts(1), ts(0.99), []string{"car"}, nil, nil),
			review("r-alert1", "driveway", "alert", ts(12.75), ts(12.74), []string{"person"}, []string{"driveway"}, nil),
			review("r-alert2", "front_door", "alert", ts(4), 0, []string{"person", "car", "person"}, []string{"porch", "street"},
				map[string]any{"title": "Courier drops off a package", "shortSummary": "A courier walks up to the porch, leaves a box and returns to a van.", "potential_threat_level": 0}),
			review("r-hidden", "bedroom", "alert", ts(2), ts(1.9), []string{"person"}, nil, nil, "e-nosnap"),
		})))
	case p == "/api/review/r-alert1":
		w.Write(mustJSON(review("r-alert1", "driveway", "alert", ts(12.75), ts(12.74), []string{"person"}, []string{"driveway"}, nil)))
	case p == "/api/review/r-hidden":
		w.Write(mustJSON(review("r-hidden", "bedroom", "alert", ts(2), ts(1.9), []string{"person"}, nil, nil)))
	case strings.HasPrefix(p, "/api/review/"):
		http.Error(w, `{"message":"Review not found"}`, http.StatusNotFound)
	case p == "/api/events" || p == "/api/events/search":
		evs := []map[string]any{
			event("e-car", "driveway", "car", ts(1), ts(0.9), []string{"driveway"}, true, "ABC123"),
			event("e-person", "front_door", "person", ts(3), 0, []string{"porch"}, false, ""),
		}
		// Like Frigate, only the asked-for labels.
		if labels := r.URL.Query().Get("labels"); labels != "" {
			var keep []map[string]any
			for _, e := range evs {
				if strings.Contains(","+labels+",", ","+e["label"].(string)+",") {
					keep = append(keep, e)
				}
			}
			evs = keep
		}
		w.Write(mustJSON(evs))
	case p == "/api/sub_labels":
		w.Write([]byte(`["Alex", "ABC123"]`))
	case strings.HasSuffix(p, "/latest.jpg") && (p == "/api/front_door/latest.jpg" || p == "/api/driveway/latest.jpg"):
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(fakeJPEG)
	case p == "/api/events/e-car":
		w.Write(mustJSON(event("e-car", "driveway", "car", ts(1), ts(0.9), []string{"driveway"}, true, "ABC123")))
	case p == "/api/events/e-person":
		w.Write(mustJSON(event("e-person", "front_door", "person", ts(3), 0, []string{"porch"}, false, "")))
	case p == "/api/events/e-a1":
		e := event("e-a1", "driveway", "person", ts(12.75), ts(12.74), []string{"driveway"}, true, "")
		if f.farAway {
			e["data"].(map[string]any)["box"] = []float64{0.7, 0.3, 0.04, 0.12}
		}
		w.Write(mustJSON(e))
	case strings.HasSuffix(p, "/snapshot.jpg") && (strings.HasPrefix(p, "/api/events/e-car/") || strings.HasPrefix(p, "/api/events/e-a1/")),
		strings.Contains(p, "/recordings/") && !f.noRecs:
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(fakeJPEG)
	default:
		http.Error(w, `{"success":false,"message":"Not found"}`, http.StatusNotFound)
	}
}

// after keeps the items that started after the request's "after" parameter, like Frigate.
func after(r *http.Request, items []map[string]any) []map[string]any {
	from, _ := strconv.ParseFloat(r.URL.Query().Get("after"), 64)
	out := []map[string]any{}
	for _, it := range items {
		if it["start_time"].(float64) >= from {
			out = append(out, it)
		}
	}
	return out
}

// review builds a review item whose objects are detections (default e-a1, which has a snapshot).
func review(id, cam, sev string, start, end float64, objects, zones []string, meta map[string]any, detections ...string) map[string]any {
	if len(detections) == 0 {
		detections = []string{"e-a1"}
	}
	r := map[string]any{"id": id, "camera": cam, "severity": sev, "start_time": start, "end_time": nil,
		"has_been_reviewed": false, "thumb_path": "/media/frigate/clips/review/thumb-" + id + ".webp",
		"data": map[string]any{"detections": detections, "objects": objects, "sub_labels": []string{},
			"zones": zones, "audio": []string{}, "thumb_time": start + 2, "metadata": meta}}
	if end > 0 {
		r["end_time"] = end
	}
	return r
}

// event builds a tracked object. Cars are far away (small in the frame); everything else is close.
func event(id, cam, label string, start, end float64, zones []string, snap bool, plate string) map[string]any {
	box := []float64{0.2, 0.1, 0.5, 0.8}
	if label == "car" {
		box = []float64{0.6, 0.4, 0.1, 0.08}
	}
	e := map[string]any{"id": id, "camera": cam, "label": label, "sub_label": nil, "zones": zones,
		"start_time": start, "end_time": nil, "has_snapshot": snap, "has_clip": true,
		"data": map[string]any{"score": 0.81, "top_score": 0.86, "recognized_license_plate": plate, "box": box}}
	if end > 0 {
		e["end_time"] = end
	}
	return e
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func start(t *testing.T, cfg config, fake *fakeFrigate) *mcp.ClientSession {
	t.Helper()
	if fake == nil {
		fake = &fakeFrigate{}
	}
	g := httptest.NewServer(fake)
	t.Cleanup(g.Close)
	if cfg.opt.Allowed == nil {
		cfg.opt.Allowed = map[string]bool{}
	}
	cfg.opt.Location = time.UTC
	cfg.opt.Now = func() time.Time { return now }
	cfg.opt.SnapshotHeight = 720
	src := frigate.New(g.URL, "viewer", "pw", 5*time.Second, false)
	h, _ := newHandler(context.Background(), src, cfg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return connect(t, srv.URL, cfg.token)
}

func connect(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func imageOf(res *mcp.CallToolResult) []byte {
	for _, c := range res.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			return ic.Data
		}
	}
	return nil
}

func toolNames(t *testing.T, cs *mcp.ClientSession) map[string]bool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range res.Tools {
		names[tool.Name] = true
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s not marked read-only", tool.Name)
		}
	}
	return names
}

func TestToolsAreListedReadOnly(t *testing.T) {
	names := toolNames(t, start(t, config{}, nil))
	want := []string{"get_alerts", "get_alerts_grid", "get_alert_snapshot", "get_events", "get_events_grid", "get_event_snapshot",
		"get_system_status", "get_camera_frame", "last_seen"}
	for _, n := range want {
		if !names[n] {
			t.Errorf("missing %s", n)
		}
	}
	if len(names) != len(want) {
		t.Fatalf("tools: %v (search_events must only appear with semantic search)", names)
	}
}

func TestSearchOnlyWithSemanticSearch(t *testing.T) {
	cs := start(t, config{}, &fakeFrigate{semantic: true})
	if names := toolNames(t, cs); !names["search_events"] || !names["search_events_grid"] {
		t.Fatalf("search tools missing although semantic search is enabled: %v", names)
	}
	res := call(t, cs, "search_events", map[string]any{"query": "red truck", "camera": "Driveway"})
	if res.IsError || !strings.Contains(text(res), `Best matches for "red truck", all retained history`) {
		t.Fatalf("got %s", text(res))
	}
}

func TestAlertsSummary(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "get_alerts", nil)
	got := text(res)
	for _, s := range []string{
		"Last 24 hours, all cameras: 3 alerts, 1 detection. Times are UTC.",
		"- today 11:00 (ongoing), front_door: person, car; zones: porch, street; id r-alert2\n",
		"  Frigate's description: Courier drops off a package. A courier walks up to the porch, leaves a box and returns to a van.\n",
		"- today 02:15 (36s), driveway: person; zones: driveway; id r-alert1\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "r-detect") {
		t.Errorf("detections listed by default:\n%s", got)
	}
	if strings.Index(got, "r-alert2") > strings.Index(got, "r-alert1") {
		t.Errorf("not newest first:\n%s", got)
	}
	// Frigate is asked for the last 24 hours, both severities (for the counts).
	q := fake.requests[len(fake.requests)-1]
	if !strings.HasPrefix(q, "/api/review?") || strings.Contains(q, "severity") ||
		!strings.Contains(q, "after="+strconv.FormatFloat(ts(24), 'f', 3, 64)) {
		t.Errorf("review query: %s", q)
	}
}

func TestAlertsAllWithLimit(t *testing.T) {
	cs := start(t, config{}, nil)
	got := text(call(t, cs, "get_alerts", map[string]any{"severity": "all", "limit": 2}))
	if !strings.Contains(got, "Alerts and detections (newest 2 of 4):") ||
		!strings.Contains(got, "today 14:00 (36s), driveway: detection, car; id r-detect") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestAllowlistHidesCameras(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{opt: tools.Options{Allowed: map[string]bool{"driveway": true, "front_door": true}}}, fake)
	got := text(call(t, cs, "get_alerts", nil))
	if strings.Contains(got, "bedroom") || !strings.Contains(got, "2 alerts, 1 detection") {
		t.Fatalf("hidden camera shown:\n%s", got)
	}
	if q := fake.requests[len(fake.requests)-1]; !strings.Contains(q, "cameras=driveway%2Cfront_door") {
		t.Errorf("allowlist not passed to frigate: %s", q)
	}
	res := call(t, cs, "get_alert_snapshot", map[string]any{"id": "r-hidden"})
	if !res.IsError || imageOf(res) != nil || !strings.Contains(text(res), `no alert or detection with id "r-hidden"`) {
		t.Fatalf("hidden camera's alert served: %s", text(res))
	}
	res = call(t, cs, "get_alerts", map[string]any{"camera": "bedroom"})
	if !res.IsError || !strings.Contains(text(res), `unknown camera "bedroom"; available: driveway, front_door`) {
		t.Fatalf("got %s", text(res))
	}
}

func TestCameraNameMatching(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "get_alerts", map[string]any{"camera": "Front Door", "since": "2d"})
	if res.IsError || !strings.Contains(text(res), "Last 2 days, front_door: 3 alerts") {
		t.Fatalf("got %s", text(res))
	}
	res = call(t, cs, "get_alerts", map[string]any{"camera": "garage"})
	if !res.IsError || !strings.Contains(text(res), "available: bedroom, driveway, front_door") {
		t.Fatalf("disabled camera listed or bad error: %s", text(res))
	}
}

func TestAlertSnapshot(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "get_alert_snapshot", map[string]any{"id": "r-alert1"})
	if res.IsError || string(imageOf(res)) != string(fakeJPEG) || text(res) != "Alert on driveway at today 02:15: person" {
		t.Fatalf("got %s", text(res))
	}
	if q := fake.requests[len(fake.requests)-1]; !strings.HasPrefix(q, "/api/driveway/recordings/"+strconv.FormatFloat(ts(12.75)+2, 'f', 3, 64)+"/snapshot.jpg?height=720") {
		t.Errorf("expected recording frame at thumb_time: %s", q)
	}
}

func TestAlertSnapshotFallsBackToEventSnapshot(t *testing.T) {
	fake := &fakeFrigate{noRecs: true}
	cs := start(t, config{}, fake)
	res := call(t, cs, "get_alert_snapshot", map[string]any{"id": "r-alert1"})
	if res.IsError || imageOf(res) == nil {
		t.Fatalf("got %s", text(res))
	}
	if q := fake.requests[len(fake.requests)-1]; q != "/api/events/e-a1/snapshot.jpg?bbox=1&height=720" {
		t.Errorf("expected event snapshot fallback: %s", q)
	}
	res = call(t, cs, "get_alert_snapshot", map[string]any{"id": "nope"})
	if !res.IsError || !strings.Contains(text(res), "get ids from get_alerts") {
		t.Fatalf("got %s", text(res))
	}
}

func decodeJPEG(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	return img
}

func TestAlertsGrid(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "get_alerts_grid", nil)
	got := text(res)
	if res.IsError {
		t.Fatalf("error: %s", got)
	}
	for _, s := range []string{
		"Last 24 hours, all cameras: 3 alerts, 1 detection. Times are UTC.\n",
		"Pictures numbered as in the grid (left to right, top to bottom), newest first:\n",
		"1. today 13:00 (6m00s), bedroom: person; id r-hidden\n",
		"2. today 11:00 (ongoing), front_door: person, car; zones: porch, street; id r-alert2\n",
		"  Frigate's description: Courier drops off a package.",
		"3. today 02:15 (36s), driveway: person; zones: driveway; id r-alert1\n",
		"Use get_alert_snapshot with an id for a bigger picture of one.\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if strings.Contains(got, "r-detect") || strings.Contains(got, "no picture") {
		t.Errorf("unexpected content:\n%s", got)
	}
	// 3 pictures: a 2x2 grid of 16:9 tiles within 1280x720.
	if b := decodeJPEG(t, imageOf(res)).Bounds(); b.Dx() != 1276 || b.Dy() != 718 {
		t.Errorf("grid size %v", b)
	}
	// Each tile's frame is asked for at the tile height, not the full snapshot height.
	frames := 0
	for _, q := range fake.requests {
		if strings.Contains(q, "/recordings/") {
			frames++
			if !strings.HasSuffix(q, "?height=357") {
				t.Errorf("frame request %s", q)
			}
		}
	}
	if frames != 3 {
		t.Errorf("%d frame requests", frames)
	}
}

func TestAlertsGridLimitAndMissingPictures(t *testing.T) {
	cs := start(t, config{}, &fakeFrigate{noRecs: true})
	res := call(t, cs, "get_alerts_grid", map[string]any{"severity": "all", "limit": 2})
	got := text(res)
	// Without recordings, r-hidden's object has no snapshot; the others fall back to e-a1's.
	for _, s := range []string{
		"Pictures of the newest 2 of 4, numbered as in the grid",
		"1. today 14:00 (36s), driveway: detection, car; id r-detect\n",
		"2. today 13:00 (6m00s), bedroom: alert, person; id r-hidden\n",
		"1 item has no picture (shown as an empty tile).\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if b := decodeJPEG(t, imageOf(res)).Bounds(); b.Dx() != 1280 || b.Dy() != 358 {
		t.Errorf("grid size %v", b)
	}

	// Only bedroom visible: its one alert has no picture, so no image is returned at all.
	bedroom := start(t, config{opt: tools.Options{Allowed: map[string]bool{"bedroom": true}}}, &fakeFrigate{noRecs: true})
	res = call(t, bedroom, "get_alerts_grid", nil)
	if res.IsError || imageOf(res) != nil || !strings.Contains(text(res), "No pictures are available for these") {
		t.Fatalf("got %s", text(res))
	}
	res = call(t, cs, "get_alerts_grid", map[string]any{"since": "30m"})
	if res.IsError || imageOf(res) != nil || !strings.Contains(text(res), "Nothing to show.") {
		t.Fatalf("got %s", text(res))
	}
	res = call(t, cs, "get_alerts_grid", map[string]any{"severity": "loud"})
	if !res.IsError || !strings.Contains(text(res), "use alert, detection or all") {
		t.Fatalf("got %s", text(res))
	}
}

func TestAlertsGridPrefersBoxedSnapshots(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{opt: tools.Options{GridBoxes: true}}, fake)
	res := call(t, cs, "get_alerts_grid", nil)
	if res.IsError || imageOf(res) == nil || strings.Contains(text(res), "no picture") {
		t.Fatalf("got %s", text(res))
	}
	var snaps, frames []string
	for _, q := range fake.requests {
		switch {
		case strings.Contains(q, "/snapshot.jpg") && strings.HasPrefix(q, "/api/events/"):
			snaps = append(snaps, q)
		case strings.Contains(q, "/recordings/"):
			frames = append(frames, q)
		}
	}
	// r-alert2 and r-alert1 have a boxed snapshot (e-a1); r-hidden's object has none, so its
	// recorded frame is used instead.
	count := map[string]int{}
	for _, q := range snaps {
		count[q]++
	}
	if len(snaps) != 3 || count["/api/events/e-a1/snapshot.jpg?bbox=1&height=357"] != 2 ||
		count["/api/events/e-nosnap/snapshot.jpg?bbox=1&height=357"] != 1 {
		t.Errorf("snapshot requests: %v", snaps)
	}
	if len(frames) != 1 || !strings.HasPrefix(frames[0], "/api/bedroom/recordings/") {
		t.Errorf("frame requests: %v", frames)
	}
}

func TestEventsGrid(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "get_events_grid", map[string]any{"since": "today"})
	got := text(res)
	for _, s := range []string{
		"Tracked objects, today. Times are UTC.\n",
		"Pictures numbered as in the grid (left to right, top to bottom), newest first:\n",
		"1. today 14:00 (6m00s), driveway: car 86%; zones: driveway; plate: ABC123; id e-car\n",
		"2. today 12:00 (ongoing), front_door: person 86%; zones: porch; no snapshot; id e-person\n",
		"1 item has no picture (shown as an empty tile).\n",
		"Use get_event_snapshot with an id for a bigger picture of one.\n",
	} {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	if b := decodeJPEG(t, imageOf(res)).Bounds(); b.Dx() != 1280 || b.Dy() != 358 {
		t.Errorf("grid size %v", b)
	}
	// Only the object with a snapshot is fetched, with its box, at tile height; the limit asked of
	// Frigate is the grid's.
	var fetched []string
	for _, q := range fake.requests {
		if strings.HasSuffix(strings.Split(q, "?")[0], ".jpg") {
			fetched = append(fetched, q)
		}
		if strings.HasPrefix(q, "/api/events?") && !strings.Contains(q, "limit=6") {
			t.Errorf("events query %s", q)
		}
	}
	// The car is far away (small in the frame), so its tile zooms in on it.
	if len(fetched) != 1 || fetched[0] != "/api/events/e-car/snapshot.jpg?bbox=1&crop=1&height=358" {
		t.Errorf("pictures fetched: %v", fetched)
	}

	res = call(t, cs, "get_events_grid", map[string]any{"label": "unicorn"})
	if !res.IsError || !strings.Contains(text(res), "tracked labels: car, dog, person, speech") {
		t.Fatalf("got %s", text(res))
	}
	res = call(t, cs, "get_events_grid", map[string]any{"limit": 2})
	if !strings.Contains(text(res), "Pictures of the newest 2 (there may be more)") {
		t.Fatalf("got %s", text(res))
	}
}

func TestSearchGrid(t *testing.T) {
	cs := start(t, config{}, &fakeFrigate{semantic: true})
	res := call(t, cs, "search_events_grid", map[string]any{"query": "red truck"})
	got := text(res)
	if res.IsError || imageOf(res) == nil ||
		!strings.Contains(got, `Best matches for "red truck", all retained history.`) ||
		!strings.Contains(got, "Pictures of the best matches first") ||
		!strings.Contains(got, "1. today 14:00 (6m00s), driveway: car") {
		t.Fatalf("got %s", got)
	}
	res = call(t, cs, "search_events_grid", map[string]any{"query": " "})
	if !res.IsError || !strings.Contains(text(res), "query is required") {
		t.Fatalf("got %s", text(res))
	}
}

func TestTimeOfDayAndOlderHint(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)

	// "around noon": from/to go to Frigate as after/before.
	res := call(t, cs, "get_events", map[string]any{"label": "car", "from": "11:30", "to": "12:30"})
	if res.IsError || !strings.Contains(text(res), "Tracked car objects, between today 11:30 and today 12:30. Times are UTC.") {
		t.Fatalf("got %s", text(res))
	}
	q := fake.requests[len(fake.requests)-1]
	noon := func(h, m int) string {
		return strconv.FormatFloat(float64(time.Date(2026, 11, 1, h, m, 0, 0, time.UTC).Unix()), 'f', 3, 64)
	}
	if !strings.Contains(q, "after="+noon(11, 30)) || !strings.Contains(q, "before="+noon(12, 30)) {
		t.Errorf("events query %s", q)
	}

	// When the limit is hit, the answer says exactly how to get the older ones.
	hint := "For older ones, call again with the same arguments and to: 12:00:00\n"
	for _, tool := range []string{"get_events", "get_events_grid"} {
		if got := text(call(t, cs, tool, map[string]any{"limit": 2})); !strings.Contains(got, hint) {
			t.Errorf("%s: no older hint in:\n%s", tool, got)
		}
		if got := text(call(t, cs, tool, map[string]any{"limit": 5})); strings.Contains(got, "For older ones") {
			t.Errorf("%s: older hint although everything was shown:\n%s", tool, got)
		}
	}
	for _, tool := range []string{"get_alerts", "get_alerts_grid"} {
		got := text(call(t, cs, tool, map[string]any{"limit": 1}))
		if !strings.Contains(got, "For older ones, call again with the same arguments and to: 13:00:00\n") {
			t.Errorf("%s: no older hint in:\n%s", tool, got)
		}
	}

	res = call(t, cs, "get_alerts", map[string]any{"from": "after lunch"})
	if !res.IsError || !strings.Contains(text(res), "use e.g. 11:30, 2pm, noon") {
		t.Fatalf("got %s", text(res))
	}
}

func TestSystemStatus(t *testing.T) {
	cs := start(t, config{}, nil)
	res := call(t, cs, "get_system_status", nil)
	want := `Frigate 0.18.1 (version 0.18.2 is available), running for 3 days 4 hours. Checked 15:00 UTC.
Cameras: 2 of 3 OK.
- driveway: OFFLINE (no video); 12 reconnects in the last hour
- OK: bedroom (object detection off), front_door
Detector coral: 8.52 ms per detection.
Storage for recordings and snapshots and clips: 186.3 GB free of 3.6 TB (95% used); low on space, Frigate deletes the oldest recordings to make room.
`
	if res.IsError || text(res) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", text(res), want)
	}

	// The allowlist hides cameras from the status too.
	cs = start(t, config{opt: tools.Options{Allowed: map[string]bool{"front_door": true}}}, nil)
	if got := text(call(t, cs, "get_system_status", nil)); !strings.Contains(got, "Camera: front_door is OK.\n") ||
		strings.Contains(got, "driveway") || strings.Contains(got, "bedroom") {
		t.Fatalf("got:\n%s", got)
	}
}

// snapshotRequests lists the object snapshots asked of the fake Frigate.
func snapshotRequests(fake *fakeFrigate) []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []string
	for _, q := range fake.requests {
		if strings.HasPrefix(q, "/api/events/") && strings.Contains(q, "/snapshot.jpg") {
			out = append(out, q)
		}
	}
	return out
}

func TestGridCrop(t *testing.T) {
	// Alerts grid, auto: the alert's object is small in the frame, so its snapshot is cropped.
	fake := &fakeFrigate{farAway: true}
	cs := start(t, config{opt: tools.Options{GridBoxes: true}}, fake)
	call(t, cs, "get_alerts_grid", map[string]any{"camera": "driveway"})
	n := 0
	for _, q := range snapshotRequests(fake) {
		if strings.HasPrefix(q, "/api/events/e-a1/") {
			n++
			if !strings.Contains(q, "crop=1") {
				t.Errorf("far-away object not cropped: %s", q)
			}
		}
	}
	if n == 0 {
		t.Fatal("no snapshot of the far-away object asked for")
	}
	// A close object (the default e-a1) isn't.
	fake = &fakeFrigate{}
	cs = start(t, config{opt: tools.Options{GridBoxes: true}}, fake)
	call(t, cs, "get_alerts_grid", map[string]any{"camera": "driveway"})
	for _, q := range snapshotRequests(fake) {
		if strings.Contains(q, "crop=1") {
			t.Errorf("close object cropped: %s", q)
		}
	}

	// GRID_CROP=off never crops, always always does; single pictures are never cropped.
	fake = &fakeFrigate{}
	cs = start(t, config{opt: tools.Options{GridCrop: tools.CropOff}}, fake)
	call(t, cs, "get_events_grid", nil)
	if q := snapshotRequests(fake); len(q) != 1 || strings.Contains(q[0], "crop") {
		t.Errorf("off: %v", q)
	}
	fake = &fakeFrigate{}
	cs = start(t, config{opt: tools.Options{GridCrop: tools.CropAlways}}, fake)
	call(t, cs, "get_events_grid", nil)
	call(t, cs, "get_event_snapshot", map[string]any{"id": "e-car"})
	if q := snapshotRequests(fake); len(q) != 2 || !strings.Contains(q[0], "crop=1") || strings.Contains(q[1], "crop") {
		t.Errorf("always: %v", q)
	}
}

func TestAlertsGridHonorsAllowlist(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{opt: tools.Options{Allowed: map[string]bool{"driveway": true}}}, fake)
	got := text(call(t, cs, "get_alerts_grid", map[string]any{"severity": "all"}))
	if strings.Contains(got, "bedroom") || strings.Contains(got, "front_door") || !strings.Contains(got, "2. today 02:15") {
		t.Fatalf("got:\n%s", got)
	}
	for _, q := range fake.requests {
		if strings.Contains(q, "/api/bedroom/") || strings.Contains(q, "/api/front_door/") {
			t.Errorf("hidden camera's picture fetched: %s", q)
		}
	}
}

func TestListEvents(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "get_events", map[string]any{"label": "cars", "zone": "Driveway", "since": "today"})
	got := text(res)
	if res.IsError || !strings.Contains(got, "Tracked car objects, today, zone driveway. Times are UTC.") ||
		!strings.Contains(got, "1 found, newest first:\n") ||
		!strings.Contains(got, "- today 14:00 (6m00s), driveway: car 86%; zones: driveway; plate: ABC123; id e-car") ||
		strings.Contains(got, "e-person") {
		t.Fatalf("got:\n%s", got)
	}
	q := lastRequest(fake, "/api/events?")
	for _, want := range []string{"labels=car", "zones=driveway", "include_thumbnails=0", "limit=10",
		"after=" + strconv.FormatFloat(ts(15), 'f', 3, 64)} {
		if !strings.Contains(q, want) {
			t.Errorf("events query %s lacks %s", q, want)
		}
	}
	// An object without a snapshot says so.
	if got := text(call(t, cs, "get_events", map[string]any{"label": "person"})); !strings.Contains(got, "front_door: person 86%; zones: porch; no snapshot; id e-person") {
		t.Fatalf("person:\n%s", got)
	}
	res = call(t, cs, "get_events", map[string]any{"label": "unicorn"})
	if !res.IsError || !strings.Contains(text(res), "tracked labels: car, dog, person, speech") {
		t.Fatalf("got %s", text(res))
	}
	res = call(t, cs, "get_events", map[string]any{"since": "sometime"})
	if !res.IsError || !strings.Contains(text(res), "use e.g. 30m, 6h") {
		t.Fatalf("got %s", text(res))
	}
}

func TestEventSnapshot(t *testing.T) {
	cs := start(t, config{}, nil)
	res := call(t, cs, "get_event_snapshot", map[string]any{"id": "e-car"})
	if res.IsError || imageOf(res) == nil || text(res) != "Car on driveway at today 14:00" {
		t.Fatalf("got %s", text(res))
	}
	res = call(t, cs, "get_event_snapshot", map[string]any{"id": "e-person"})
	if !res.IsError || !strings.Contains(text(res), "no snapshot was saved for e-person") {
		t.Fatalf("got %s", text(res))
	}
}

func TestLogsInAgainWhenSessionExpires(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	call(t, cs, "get_alerts", nil)
	fake.mu.Lock()
	fake.token = "revoked.token.value" // the session expires on Frigate's side
	fake.mu.Unlock()
	res := call(t, cs, "get_events", nil)
	if res.IsError {
		t.Fatalf("got %s", text(res))
	}
	if fake.logins != 2 {
		t.Fatalf("logins: %d", fake.logins)
	}
}

func TestBadLoginIsReported(t *testing.T) {
	g := httptest.NewServer(&fakeFrigate{})
	t.Cleanup(g.Close)
	err := frigate.New(g.URL, "viewer", "wrong", time.Second, false).Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "wrong FRIGATE_USERNAME or FRIGATE_PASSWORD") {
		t.Fatalf("got %v", err)
	}
	err = frigate.New(g.URL, "", "", time.Second, false).Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "use the internal port 5000") {
		t.Fatalf("got %v", err)
	}
}

func TestErrorsDontLeakDetails(t *testing.T) {
	g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "db error at /media/frigate/frigate.db", http.StatusInternalServerError)
	}))
	t.Cleanup(g.Close)
	cfg := config{opt: tools.Options{Allowed: map[string]bool{}, Location: time.UTC}}
	h, _ := newHandler(context.Background(), frigate.New(g.URL, "", "", time.Second, false), cfg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	res := call(t, connect(t, srv.URL, ""), "get_alerts", nil)
	if !res.IsError || strings.Contains(text(res), "frigate.db") || !strings.Contains(text(res), "details in the frigate-mcp log") {
		t.Fatalf("got %s", text(res))
	}
}

func TestAuthToken(t *testing.T) {
	g := httptest.NewServer(&fakeFrigate{})
	t.Cleanup(g.Close)
	cfg := config{token: "s3cret", opt: tools.Options{Allowed: map[string]bool{}}}
	h, _ := newHandler(context.Background(), frigate.New(g.URL, "viewer", "pw", time.Second, false), cfg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: got %d", resp.StatusCode)
	}
	connect(t, srv.URL, "s3cret") // the right token works
}

func TestHealthz(t *testing.T) {
	g := httptest.NewServer(&fakeFrigate{})
	cfg := config{opt: tools.Options{Allowed: map[string]bool{}}}
	h, _ := newHandler(context.Background(), frigate.New(g.URL, "viewer", "pw", time.Second, false), cfg)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	for _, want := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		resp, err := http.Get(srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("healthz: got %d, want %d", resp.StatusCode, want)
		}
		g.Close()
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("FRIGATE_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing FRIGATE_URL accepted")
	}
	t.Setenv("FRIGATE_URL", "https://frigate:8971")
	t.Setenv("FRIGATE_USERNAME", "viewer")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("username without password: %v", err)
	}
	t.Setenv("FRIGATE_PASSWORD", "pw")
	t.Setenv("CAMERAS", "Driveway, front_door")
	t.Setenv("FRIGATE_TLS_INSECURE", "true")
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !c.insecureTLS || !c.opt.Allowed["driveway"] || !c.opt.Allowed["front_door"] || c.opt.SnapshotHeight != 720 || c.listen != ":8091" ||
		c.opt.GridCamera || !c.opt.GridBoxes {
		t.Fatalf("got %+v", c)
	}
	if c.opt.GridLabelCorner != tools.BottomLeft {
		t.Fatalf("default label position: %q", c.opt.GridLabelCorner)
	}
	t.Setenv("GRID_LABEL_POSITION", "Top Right")
	if c, err = loadConfig(); err != nil || c.opt.GridLabelCorner != tools.TopRight {
		t.Fatalf("GRID_LABEL_POSITION=Top Right: %q %v", c.opt.GridLabelCorner, err)
	}
	t.Setenv("GRID_LABEL_POSITION", "middle")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "use top-left, top-right") {
		t.Fatalf("GRID_LABEL_POSITION=middle: %v", err)
	}
	t.Setenv("GRID_LABEL_POSITION", "")
	if c.opt.GridCrop != tools.CropAuto {
		t.Fatalf("default crop: %q", c.opt.GridCrop)
	}
	t.Setenv("GRID_CROP", "Always")
	if c, err = loadConfig(); err != nil || c.opt.GridCrop != tools.CropAlways {
		t.Fatalf("GRID_CROP=Always: %q %v", c.opt.GridCrop, err)
	}
	t.Setenv("GRID_CROP", "sometimes")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "GRID_CROP: \"sometimes\" not understood; use auto, always or off") {
		t.Fatalf("GRID_CROP=sometimes: %v", err)
	}
	t.Setenv("GRID_CROP", "")
	t.Setenv("GRID_BOXES", "false")
	if c, err = loadConfig(); err != nil || c.opt.GridBoxes {
		t.Fatalf("GRID_BOXES=false: %+v %v", c.opt, err)
	}
	t.Setenv("GRID_BOXES", "")
	t.Setenv("GRID_CAMERA_LABELS", "true")
	if c, err = loadConfig(); err != nil || !c.opt.GridCamera {
		t.Fatalf("GRID_CAMERA_LABELS=true: %+v %v", c.opt, err)
	}
	t.Setenv("GRID_CAMERA_LABELS", "maybe")
	if _, err := loadConfig(); err == nil {
		t.Fatal("GRID_CAMERA_LABELS=maybe accepted")
	}
	t.Setenv("GRID_CAMERA_LABELS", "")
	t.Setenv("SNAPSHOT_HEIGHT", "-1")
	if _, err := loadConfig(); err == nil {
		t.Fatal("negative SNAPSHOT_HEIGHT accepted")
	}
}

// lastRequest is the last API request the fake Frigate got whose path starts with prefix.
func lastRequest(fake *fakeFrigate, prefix string) string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for i := len(fake.requests) - 1; i >= 0; i-- {
		if strings.HasPrefix(fake.requests[i], prefix) {
			return fake.requests[i]
		}
	}
	return ""
}

func TestCameraFrame(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)

	// Now: the live frame.
	res := call(t, cs, "get_camera_frame", map[string]any{"camera": "Front Door"})
	if res.IsError || imageOf(res) == nil || text(res) != "front_door now (today 15:00)" {
		t.Fatalf("now: %s", text(res))
	}
	if q := lastRequest(fake, "/api/front_door/latest.jpg"); q != "/api/front_door/latest.jpg?height=720" {
		t.Errorf("live request: %q", q)
	}
	// A camera that's offline says so: its "latest" may be an old picture.
	res = call(t, cs, "get_camera_frame", map[string]any{"camera": "driveway", "at": "now"})
	if res.IsError || !strings.Contains(text(res), "driveway seems to be offline") {
		t.Fatalf("offline: %s", text(res))
	}

	// A past time: from the recordings.
	res = call(t, cs, "get_camera_frame", map[string]any{"camera": "front_door", "at": "11:30"})
	if res.IsError || imageOf(res) == nil || text(res) != "front_door at today 11:30" {
		t.Fatalf("past: %s", text(res))
	}
	want := "/api/front_door/recordings/" + strconv.FormatFloat(ts(3.5), 'f', 3, 64) + "/snapshot.jpg?height=720"
	if q := lastRequest(fake, "/api/front_door/recordings/"); q != want {
		t.Errorf("recording request %q, want %q", q, want)
	}

	// Then and now, side by side in one picture.
	res = call(t, cs, "get_camera_frame", map[string]any{"camera": "front_door", "at": "10:00", "compare": "now"})
	got := text(res)
	if res.IsError || !strings.Contains(got, "front_door, side by side (left to right):\n1. at today 10:00\n2. now (today 15:00)\n") {
		t.Fatalf("compare: %s", got)
	}
	if b := decodeJPEG(t, imageOf(res)).Bounds(); b.Dx() != 1280 || b.Dy() != 358 {
		t.Errorf("compare size %v", b)
	}

	for args, want := range map[string]map[string]any{
		"is in the future":               {"camera": "front_door", "at": "today 18:00"},
		`at "lunchtime" not understood`:  {"camera": "front_door", "at": "lunchtime"},
		`unknown camera "attic"`:         {"camera": "attic"},
		`missing properties: ["camera"]`: {},
		"camera is required":             {"camera": " "},
	} {
		res := call(t, cs, "get_camera_frame", want)
		if !res.IsError || !strings.Contains(text(res), args) {
			t.Errorf("%v: got %s", want, text(res))
		}
	}

	// No recording at that time.
	cs = start(t, config{}, &fakeFrigate{noRecs: true})
	res = call(t, cs, "get_camera_frame", map[string]any{"camera": "front_door", "at": "03:00"})
	if !res.IsError || !strings.Contains(text(res), "no recording of front_door at today 03:00") {
		t.Fatalf("no recording: %s", text(res))
	}
	// One side missing: the other is still shown.
	res = call(t, cs, "get_camera_frame", map[string]any{"camera": "front_door", "at": "03:00", "compare": "now"})
	if res.IsError || imageOf(res) == nil || !strings.Contains(text(res), "1. at today 03:00: no picture: no recording") {
		t.Fatalf("half missing: %s", text(res))
	}
}

func TestLastSeen(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "last_seen", map[string]any{"label": "cars"})
	if got := text(res); res.IsError || got != "Last car: today 14:00 on driveway (1 hour ago), for 6m00s. Zones: driveway. Plate ABC123. Id e-car.\n"+
		"Use get_event_snapshot with the id to see it.\n" {
		t.Fatalf("got %q", got)
	}
	q := lastRequest(fake, "/api/events?")
	for _, want := range []string{"labels=car", "limit=1", "after=" + strconv.FormatFloat(ts(31*24), 'f', 3, 64)} {
		if !strings.Contains(q, want) {
			t.Errorf("query %s lacks %s", q, want)
		}
	}

	// Still there.
	res = call(t, cs, "last_seen", map[string]any{"label": "person", "camera": "front_door"})
	if got := text(res); got != "Person on front_door is there now, since today 12:00 (3 hours ago). Zones: porch. Id e-person.\n" {
		t.Fatalf("ongoing: %q", got)
	}
	// Without a camera in the question, the answer names it.
	res = call(t, cs, "last_seen", map[string]any{"label": "person"})
	if got := text(res); !strings.HasPrefix(got, "Person is there now on front_door, since today 12:00") {
		t.Fatalf("ongoing, any camera: %q", got)
	}

	// A recognized name, matched loosely; an unknown one lists the known names.
	call(t, cs, "last_seen", map[string]any{"name": "alex", "within": "1w"})
	if q := lastRequest(fake, "/api/events?"); !strings.Contains(q, "sub_labels=Alex") || !strings.Contains(q, "after="+strconv.FormatFloat(ts(7*24), 'f', 3, 64)) {
		t.Errorf("name query: %s", q)
	}
	res = call(t, cs, "last_seen", map[string]any{"name": "Sasha"})
	if !res.IsError || !strings.Contains(text(res), `unknown name "Sasha"; names Frigate has recognized: Alex, ABC123`) {
		t.Fatalf("unknown name: %s", text(res))
	}

	// Never seen.
	res = call(t, cs, "last_seen", map[string]any{"label": "dog", "zone": "driveway"})
	if got := text(res); got != "No dog seen in zone driveway in the last 31 days.\n" {
		t.Fatalf("none: %q", got)
	}
	res = call(t, cs, "last_seen", nil)
	if !res.IsError || !strings.Contains(text(res), "give a label") {
		t.Fatalf("no args: %s", text(res))
	}
}
