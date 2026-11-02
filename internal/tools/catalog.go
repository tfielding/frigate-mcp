package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

// catalog is what Frigate's config says exists: cameras, labels and zones, used to validate
// arguments (so a model can't invent a camera) and to put real names in error messages.
type catalog struct {
	cameras  []string // visible cameras, sorted
	labels   []string // object and audio labels tracked on visible cameras, sorted
	zones    []string // zones on visible cameras, sorted
	semantic bool     // semantic search enabled
}

// catalogCache refreshes the catalog from Frigate at most once per ttl.
type catalogCache struct {
	src     Source
	allowed map[string]bool
	ttl     time.Duration

	mu      sync.Mutex
	cur     *catalog
	fetched time.Time
}

func (c *catalogCache) get(ctx context.Context) (*catalog, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && time.Since(c.fetched) < c.ttl {
		return c.cur, nil
	}
	cfg, err := c.src.Config(ctx)
	if err != nil {
		if c.cur != nil {
			return c.cur, nil // keep using the last good config while Frigate restarts
		}
		return nil, err
	}
	c.cur, c.fetched = buildCatalog(cfg, c.allowed), time.Now()
	return c.cur, nil
}

func buildCatalog(cfg *frigate.Config, allowed map[string]bool) *catalog {
	cat := &catalog{semantic: cfg.SemanticSearch.Enabled}
	labels, zones := map[string]bool{}, map[string]bool{}
	for name, cam := range cfg.Cameras {
		if cam.Enabled != nil && !*cam.Enabled {
			continue
		}
		if len(allowed) > 0 && !allowed[strings.ToLower(name)] {
			continue
		}
		cat.cameras = append(cat.cameras, name)
		for _, l := range cam.Objects.Track {
			labels[l] = true
		}
		if cam.Audio.Enabled {
			for _, l := range cam.Audio.Listen {
				labels[l] = true
			}
		}
		for z := range cam.Zones {
			zones[z] = true
		}
	}
	sort.Strings(cat.cameras)
	cat.labels, cat.zones = keys(labels), keys(zones)
	return cat
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// camera resolves a camera name the way a person might type it ("Front Door" for front_door).
func (c *catalog) camera(name string) (string, error) {
	if n, ok := match(c.cameras, name); ok {
		return n, nil
	}
	return "", fmt.Errorf("unknown camera %q; available: %s", name, list(c.cameras))
}

func (c *catalog) label(name string) (string, error) {
	if n, ok := match(c.labels, name); ok {
		return n, nil
	}
	// Plurals: "cars", "persons", "people".
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "people" {
		lower = "person"
	}
	if n, ok := match(c.labels, strings.TrimSuffix(lower, "s")); ok {
		return n, nil
	}
	if n, ok := match(c.labels, lower); ok {
		return n, nil
	}
	return "", fmt.Errorf("unknown label %q; tracked labels: %s", name, list(c.labels))
}

func (c *catalog) zone(name string) (string, error) {
	if n, ok := match(c.zones, name); ok {
		return n, nil
	}
	if len(c.zones) == 0 {
		return "", fmt.Errorf("unknown zone %q; no zones are configured", name)
	}
	return "", fmt.Errorf("unknown zone %q; available: %s", name, list(c.zones))
}

// filterCameras is the camera filter for a query: the one asked for, or every visible camera when
// an allowlist is in effect (so hidden cameras are never returned), or nil for all.
func (c *catalog) filterCameras(camera string, allowlist bool) ([]string, error) {
	if camera != "" {
		name, err := c.camera(camera)
		if err != nil {
			return nil, err
		}
		return []string{name}, nil
	}
	if allowlist {
		if len(c.cameras) == 0 {
			return nil, fmt.Errorf("no cameras available")
		}
		return c.cameras, nil
	}
	return nil, nil
}

func (c *catalog) visible(camera string) bool {
	for _, n := range c.cameras {
		if n == camera {
			return true
		}
	}
	return false
}

func match(names []string, want string) (string, bool) {
	w := normalize(want)
	for _, n := range names {
		if normalize(n) == w {
			return n, true
		}
	}
	return "", false
}

func normalize(s string) string {
	return strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(strings.TrimSpace(s)))
}

func list(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
