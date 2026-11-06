// Package frigate is a minimal read-only client for the Frigate NVR HTTP API (0.18).
package frigate

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxImageBytes = 15 << 20

// ErrNotFound is returned when Frigate has no such item (or no image for it).
var ErrNotFound = errors.New("not found")

// Client talks to Frigate's API. With a username it logs in on the authenticated port (8971) and
// sends the session token as a bearer token, logging in again when it expires. Without one it
// expects the internal unauthenticated port (5000).
type Client struct {
	base     string
	user     string
	password string
	http     *http.Client

	mu    sync.Mutex
	token string
}

// New returns a client. timeout bounds each request; insecureTLS skips certificate verification
// (Frigate's TLS port uses a self-signed certificate unless you configure your own).
func New(baseURL, user, password string, timeout time.Duration, insecureTLS bool) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if insecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for self-signed certs
	}
	return &Client{
		base:     strings.TrimRight(baseURL, "/"),
		user:     user,
		password: password,
		http: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			// Don't follow redirects: an unauthenticated request is redirected to /login.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Config is the subset of Frigate's config this server needs.
type Config struct {
	Cameras        map[string]CameraConfig `json:"cameras"`
	SemanticSearch struct {
		Enabled bool `json:"enabled"`
	} `json:"semantic_search"`
}

type CameraConfig struct {
	Enabled *bool                      `json:"enabled"`
	Zones   map[string]json.RawMessage `json:"zones"`
	Objects struct {
		Track []string `json:"track"`
	} `json:"objects"`
	Audio struct {
		Enabled bool     `json:"enabled"`
		Listen  []string `json:"listen"`
	} `json:"audio"`
}

// Review is a review item: activity Frigate grouped into an alert or a detection.
type Review struct {
	ID       string     `json:"id"`
	Camera   string     `json:"camera"`
	Start    Time       `json:"start_time"`
	End      *Time      `json:"end_time"` // nil while in progress
	Severity string     `json:"severity"` // "alert" or "detection"
	Data     ReviewData `json:"data"`
}

type ReviewData struct {
	Detections []string        `json:"detections"` // event ids
	Objects    []string        `json:"objects"`
	SubLabels  []string        `json:"sub_labels"`
	Zones      []string        `json:"zones"`
	Audio      []string        `json:"audio"`
	ThumbTime  float64         `json:"thumb_time"`
	Metadata   *ReviewMetadata `json:"metadata"` // only with GenAI review descriptions
}

// ReviewMetadata is written by Frigate's GenAI review descriptions, if configured.
type ReviewMetadata struct {
	Title        string   `json:"title"`
	ShortSummary string   `json:"shortSummary"`
	ThreatLevel  int      `json:"potential_threat_level"`
	Concerns     []string `json:"other_concerns"`
}

// Event is a tracked object.
type Event struct {
	ID          string    `json:"id"`
	Camera      string    `json:"camera"`
	Label       string    `json:"label"`
	SubLabel    SubLabel  `json:"sub_label"`
	Zones       []string  `json:"zones"`
	Start       Time      `json:"start_time"`
	End         *Time     `json:"end_time"`
	HasSnapshot bool      `json:"has_snapshot"`
	HasClip     bool      `json:"has_clip"`
	Data        EventData `json:"data"`
}

type EventData struct {
	Score       float64   `json:"score"`
	TopScore    float64   `json:"top_score"`
	Description string    `json:"description"`
	Plate       string    `json:"recognized_license_plate"`
	Box         []float64 `json:"box"` // the object in its snapshot: x, y, width, height as fractions of the frame
}

// SubLabel is a recognized face, plate owner, etc. Older Frigate versions sent [name, score].
type SubLabel string

func (s *SubLabel) UnmarshalJSON(b []byte) error {
	var str string
	if json.Unmarshal(b, &str) == nil {
		*s = SubLabel(str)
		return nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(b, &arr) == nil && len(arr) > 0 && json.Unmarshal(arr[0], &str) == nil {
		*s = SubLabel(str)
		return nil
	}
	*s = ""
	return nil
}

// Time is a Unix timestamp in seconds, as Frigate stores it.
type Time struct{ time.Time }

func (t *Time) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "null" || s == "" {
		return nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		t.Time = time.Unix(0, int64(f*1e9))
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("unexpected time %q", s)
	}
	t.Time = parsed
	return nil
}

// Stats is the subset of /api/stats this server reports. Frigate filters cameras to the ones the
// user may see.
type Stats struct {
	Cameras      map[string]CameraStats   `json:"cameras"`
	Detectors    map[string]DetectorStats `json:"detectors"`
	DetectionFPS float64                  `json:"detection_fps"`
	Service      struct {
		Uptime        int64                   `json:"uptime"` // seconds
		Version       string                  `json:"version"`
		LatestVersion string                  `json:"latest_version"`
		Storage       map[string]StorageStats `json:"storage"` // by path; sizes in MiB
	} `json:"service"`
}

type CameraStats struct {
	CameraFPS          float64 `json:"camera_fps"`
	SkippedFPS         float64 `json:"skipped_fps"`
	DetectionEnabled   bool    `json:"detection_enabled"`
	ConnectionQuality  string  `json:"connection_quality"` // excellent, fair, poor, unusable (0.17+)
	ExpectedFPS        float64 `json:"expected_fps"`
	ReconnectsLastHour int     `json:"reconnects_last_hour"`
	StallsLastHour     int     `json:"stalls_last_hour"`
}

type DetectorStats struct {
	InferenceSpeed float64  `json:"inference_speed"` // milliseconds
	Temperature    *float64 `json:"temperature"`
}

type StorageStats struct {
	Total float64 `json:"total"`
	Used  float64 `json:"used"`
	Free  float64 `json:"free"`
}

// Filter narrows a review or event query. Empty fields mean "all".
type Filter struct {
	Cameras   []string
	Labels    []string
	SubLabels []string // recognized names (faces, plates)
	Zones     []string
	After     time.Time
	Before    time.Time
	Limit     int
}

func (f Filter) values() url.Values {
	q := url.Values{}
	if len(f.Cameras) > 0 {
		q.Set("cameras", strings.Join(f.Cameras, ","))
	}
	if len(f.Labels) > 0 {
		q.Set("labels", strings.Join(f.Labels, ","))
	}
	if len(f.SubLabels) > 0 {
		q.Set("sub_labels", strings.Join(f.SubLabels, ","))
	}
	if len(f.Zones) > 0 {
		q.Set("zones", strings.Join(f.Zones, ","))
	}
	if !f.After.IsZero() {
		q.Set("after", unix(f.After))
	}
	if !f.Before.IsZero() {
		q.Set("before", unix(f.Before))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}

func unix(t time.Time) string { return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 3, 64) }

// Config returns Frigate's configuration (credentials are redacted by Frigate).
func (c *Client) Config(ctx context.Context) (*Config, error) {
	var cfg Config
	if err := c.getJSON(ctx, "/api/config", nil, 16<<20, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Reviews returns review items in a time range, newest first. severity is "alert", "detection" or "".
func (c *Client) Reviews(ctx context.Context, f Filter, severity string) ([]Review, error) {
	q := f.values()
	if severity != "" {
		q.Set("severity", severity)
	}
	var out []Review
	if err := c.getJSON(ctx, "/api/review", q, 32<<20, &out); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.After(out[j].Start.Time) })
	return out, nil
}

// Review returns one review item.
func (c *Client) Review(ctx context.Context, id string) (*Review, error) {
	var r Review
	if err := c.getJSON(ctx, "/api/review/"+url.PathEscape(id), nil, 1<<20, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Event returns one tracked object.
func (c *Client) Event(ctx context.Context, id string) (*Event, error) {
	var e Event
	if err := c.getJSON(ctx, "/api/events/"+url.PathEscape(id), nil, 4<<20, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Events returns tracked objects, newest first, without thumbnails.
func (c *Client) Events(ctx context.Context, f Filter) ([]Event, error) {
	q := f.values()
	q.Set("include_thumbnails", "0")
	var out []Event
	if err := c.getJSON(ctx, "/api/events", q, 16<<20, &out); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.After(out[j].Start.Time) })
	return out, nil
}

// Search runs a semantic search (requires semantic search to be enabled in Frigate), best match first.
func (c *Client) Search(ctx context.Context, query string, f Filter) ([]Event, error) {
	q := f.values()
	q.Set("query", query)
	q.Set("search_type", "thumbnail,description")
	q.Set("include_thumbnails", "0")
	var out []Event
	if err := c.getJSON(ctx, "/api/events/search", q, 16<<20, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EventSnapshot returns the saved snapshot of a tracked object as JPEG, with its bounding box.
// With crop, it's cut down to the area around the object.
func (c *Client) EventSnapshot(ctx context.Context, id string, height int, crop bool) ([]byte, error) {
	q := url.Values{"bbox": {"1"}}
	if height > 0 {
		q.Set("height", strconv.Itoa(height))
	}
	if crop {
		q.Set("crop", "1")
	}
	return c.image(ctx, "/api/events/"+url.PathEscape(id)+"/snapshot.jpg", q)
}

// LiveFrame returns a camera's current frame as JPEG. If the camera is offline, Frigate sends an
// older frame or a placeholder instead, without saying so: check the camera's stats.
func (c *Client) LiveFrame(ctx context.Context, camera string, height int) ([]byte, error) {
	q := url.Values{}
	if height > 0 {
		q.Set("height", strconv.Itoa(height))
	}
	return c.image(ctx, "/api/"+url.PathEscape(camera)+"/latest.jpg", q)
}

// SubLabels returns the recognized names Frigate knows (faces, plates and the like).
func (c *Client) SubLabels(ctx context.Context) ([]string, error) {
	var out []string
	if err := c.getJSON(ctx, "/api/sub_labels", nil, 1<<20, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RecordingFrame returns the recorded frame of a camera at t as JPEG (requires recordings).
func (c *Client) RecordingFrame(ctx context.Context, camera string, t float64, height int) ([]byte, error) {
	q := url.Values{}
	if height > 0 {
		q.Set("height", strconv.Itoa(height))
	}
	p := fmt.Sprintf("/api/%s/recordings/%s/snapshot.jpg", url.PathEscape(camera), strconv.FormatFloat(t, 'f', 3, 64))
	return c.image(ctx, p, q)
}

// Stats returns Frigate's current system stats (any logged-in user may read them).
func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	var s Stats
	if err := c.getJSON(ctx, "/api/stats", nil, 4<<20, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Ping checks that Frigate is reachable (and, with a username, that login works).
func (c *Client) Ping(ctx context.Context) error {
	_, _, err := c.do(ctx, "/api/config", nil, 16<<20)
	return err
}

// ReviewPreview returns an animated GIF of a review item, from a few seconds before it starts to a
// few seconds after it ends, made from Frigate's low-resolution preview recordings.
func (c *Client) ReviewPreview(ctx context.Context, id string) ([]byte, error) {
	path := "/api/review/" + url.PathEscape(id) + "/preview"
	body, ctype, err := c.do(ctx, path, url.Values{"format": {"gif"}}, maxImageBytes)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ctype, "image/gif") || !bytes.HasPrefix(body, []byte("GIF8")) {
		return nil, fmt.Errorf("frigate %s: returned %q instead of a GIF", path, ctype)
	}
	return body, nil
}

func (c *Client) image(ctx context.Context, path string, q url.Values) ([]byte, error) {
	body, ctype, err := c.do(ctx, path, q, maxImageBytes)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(ctype, "image/jpeg") || !bytes.HasPrefix(body, []byte{0xFF, 0xD8}) {
		return nil, fmt.Errorf("frigate %s: returned %q instead of a JPEG", path, ctype)
	}
	return body, nil
}

func (c *Client) getJSON(ctx context.Context, path string, q url.Values, limit int64, v any) error {
	body, _, err := c.do(ctx, path, q, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("frigate %s: unexpected response: %w", path, err)
	}
	return nil
}

// do sends a GET, logging in first if needed and once more if the session has expired.
func (c *Client) do(ctx context.Context, path string, q url.Values, limit int64) ([]byte, string, error) {
	for attempt := 0; ; attempt++ {
		token, err := c.session(ctx, attempt > 0)
		if err != nil {
			return nil, "", err
		}
		u := c.base + path
		if len(q) > 0 {
			u += "?" + q.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, "", err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, "", err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if err != nil {
			return nil, "", err
		}
		unauthorized := resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusFound
		if unauthorized && c.user != "" && attempt == 0 {
			continue // token expired or revoked: log in again
		}
		switch {
		case unauthorized:
			if c.user == "" {
				return nil, "", fmt.Errorf("frigate %s: authentication required; set FRIGATE_USERNAME and FRIGATE_PASSWORD, or use the internal port 5000", path)
			}
			return nil, "", fmt.Errorf("frigate %s: not authorized", path)
		case resp.StatusCode == http.StatusForbidden:
			return nil, "", fmt.Errorf("frigate %s: the Frigate user has no access to this", path)
		case resp.StatusCode == http.StatusNotFound:
			return nil, "", fmt.Errorf("frigate %s: %w", path, ErrNotFound)
		case resp.StatusCode != http.StatusOK:
			return nil, "", fmt.Errorf("frigate %s: %s: %s", path, resp.Status, message(body))
		case int64(len(body)) > limit:
			return nil, "", fmt.Errorf("frigate %s: response larger than %d bytes", path, limit)
		}
		return body, resp.Header.Get("Content-Type"), nil
	}
}

// session returns the bearer token, logging in when there is none or fresh is set.
func (c *Client) session(ctx context.Context, fresh bool) (string, error) {
	if c.user == "" {
		return "", nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && !fresh {
		return c.token, nil
	}
	c.token = ""
	body, _ := json.Marshal(map[string]string{"user": c.user, "password": c.password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return "", errors.New("frigate login failed: wrong FRIGATE_USERNAME or FRIGATE_PASSWORD")
	case http.StatusNotFound:
		return "", errors.New("frigate login: authentication is disabled in Frigate; leave FRIGATE_USERNAME empty")
	default:
		return "", fmt.Errorf("frigate login: %s: %s", resp.Status, message(msg))
	}
	// The session token arrives as a cookie (name configurable in Frigate, frigate_token by default).
	for _, ck := range resp.Cookies() {
		if strings.Count(ck.Value, ".") == 2 {
			c.token = ck.Value
			return c.token, nil
		}
	}
	return "", errors.New("frigate login: no session token in the response")
}

func message(body []byte) string {
	var m struct {
		Message string `json:"message"`
		Detail  any    `json:"detail"`
	}
	if json.Unmarshal(body, &m) == nil {
		if m.Message != "" {
			return m.Message
		}
		if m.Detail != nil {
			return fmt.Sprint(m.Detail)
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
