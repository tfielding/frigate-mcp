package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	fakeJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 'f', 'a', 'k', 'e', 0xFF, 0xD9}
	now      = time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)
)

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
	case p == "/api/review":
		w.Write(mustJSON([]map[string]any{
			review("r-detect", "driveway", "detection", ts(1), ts(0.99), []string{"car"}, nil, nil),
			review("r-alert1", "driveway", "alert", ts(12.75), ts(12.74), []string{"person"}, []string{"driveway"}, nil),
			review("r-alert2", "front_door", "alert", ts(4), 0, []string{"person", "car", "person"}, []string{"porch", "street"},
				map[string]any{"title": "Courier drops off a package", "shortSummary": "A courier walks up to the porch, leaves a box and returns to a van.", "potential_threat_level": 0}),
			review("r-hidden", "bedroom", "alert", ts(2), ts(1.9), []string{"person"}, nil, nil),
		}))
	case p == "/api/review/r-alert1":
		w.Write(mustJSON(review("r-alert1", "driveway", "alert", ts(12.75), ts(12.74), []string{"person"}, []string{"driveway"}, nil)))
	case p == "/api/review/r-hidden":
		w.Write(mustJSON(review("r-hidden", "bedroom", "alert", ts(2), ts(1.9), []string{"person"}, nil, nil)))
	case strings.HasPrefix(p, "/api/review/"):
		http.Error(w, `{"message":"Review not found"}`, http.StatusNotFound)
	case p == "/api/events" || p == "/api/events/search":
		w.Write(mustJSON([]map[string]any{
			event("e-car", "driveway", "car", ts(1), ts(0.9), []string{"driveway"}, true, "ABC123"),
			event("e-person", "front_door", "person", ts(3), 0, []string{"porch"}, false, ""),
		}))
	case p == "/api/events/e-car":
		w.Write(mustJSON(event("e-car", "driveway", "car", ts(1), ts(0.9), []string{"driveway"}, true, "ABC123")))
	case p == "/api/events/e-person":
		w.Write(mustJSON(event("e-person", "front_door", "person", ts(3), 0, []string{"porch"}, false, "")))
	case strings.HasSuffix(p, "/snapshot.jpg") && (strings.HasPrefix(p, "/api/events/e-car/") || strings.HasPrefix(p, "/api/events/e-a1/")),
		strings.Contains(p, "/recordings/") && !f.noRecs:
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(fakeJPEG)
	default:
		http.Error(w, `{"success":false,"message":"Not found"}`, http.StatusNotFound)
	}
}

func review(id, cam, sev string, start, end float64, objects, zones []string, meta map[string]any) map[string]any {
	r := map[string]any{"id": id, "camera": cam, "severity": sev, "start_time": start, "end_time": nil,
		"has_been_reviewed": false, "thumb_path": "/media/frigate/clips/review/thumb-" + id + ".webp",
		"data": map[string]any{"detections": []string{"e-a1"}, "objects": objects, "sub_labels": []string{},
			"zones": zones, "audio": []string{}, "thumb_time": start + 2, "metadata": meta}}
	if end > 0 {
		r["end_time"] = end
	}
	return r
}

func event(id, cam, label string, start, end float64, zones []string, snap bool, plate string) map[string]any {
	e := map[string]any{"id": id, "camera": cam, "label": label, "sub_label": nil, "zones": zones,
		"start_time": start, "end_time": nil, "has_snapshot": snap, "has_clip": true,
		"data": map[string]any{"score": 0.81, "top_score": 0.86, "recognized_license_plate": plate}}
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

func image(res *mcp.CallToolResult) []byte {
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
	want := []string{"get_alerts", "get_alert_snapshot", "list_events", "get_event_snapshot"}
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
	if !toolNames(t, cs)["search_events"] {
		t.Fatal("search_events missing although semantic search is enabled")
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
	if !res.IsError || image(res) != nil || !strings.Contains(text(res), `no alert or detection with id "r-hidden"`) {
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
	if res.IsError || string(image(res)) != string(fakeJPEG) || text(res) != "Alert on driveway at today 02:15: person" {
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
	if res.IsError || image(res) == nil {
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

func TestListEvents(t *testing.T) {
	fake := &fakeFrigate{}
	cs := start(t, config{}, fake)
	res := call(t, cs, "list_events", map[string]any{"label": "cars", "zone": "Driveway", "since": "today"})
	got := text(res)
	if res.IsError || !strings.Contains(got, "Tracked car objects, today, zone driveway. Times are UTC.") ||
		!strings.Contains(got, "- today 14:00 (6m00s), driveway: car 86%; zones: driveway; plate: ABC123; id e-car") ||
		!strings.Contains(got, "front_door: person 86%; zones: porch; no snapshot; id e-person") {
		t.Fatalf("got:\n%s", got)
	}
	q := fake.requests[len(fake.requests)-1]
	for _, want := range []string{"labels=car", "zones=driveway", "include_thumbnails=0", "limit=10",
		"after=" + strconv.FormatFloat(ts(15), 'f', 3, 64)} {
		if !strings.Contains(q, want) {
			t.Errorf("events query %s lacks %s", q, want)
		}
	}
	res = call(t, cs, "list_events", map[string]any{"label": "unicorn"})
	if !res.IsError || !strings.Contains(text(res), "tracked labels: car, dog, person, speech") {
		t.Fatalf("got %s", text(res))
	}
	res = call(t, cs, "list_events", map[string]any{"since": "sometime"})
	if !res.IsError || !strings.Contains(text(res), "use e.g. 30m, 6h") {
		t.Fatalf("got %s", text(res))
	}
}

func TestEventSnapshot(t *testing.T) {
	cs := start(t, config{}, nil)
	res := call(t, cs, "get_event_snapshot", map[string]any{"id": "e-car"})
	if res.IsError || image(res) == nil || text(res) != "Car on driveway at today 14:00" {
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
	res := call(t, cs, "list_events", nil)
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
	if !c.insecureTLS || !c.opt.Allowed["driveway"] || !c.opt.Allowed["front_door"] || c.opt.SnapshotHeight != 720 || c.listen != ":8091" {
		t.Fatalf("got %+v", c)
	}
	t.Setenv("SNAPSHOT_HEIGHT", "-1")
	if _, err := loadConfig(); err == nil {
		t.Fatal("negative SNAPSHOT_HEIGHT accepted")
	}
}
