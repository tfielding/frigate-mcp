package tools

// daily_digest: one call that sums up a period, so a model doesn't have to list alerts and events
// and add them up itself.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

const (
	digestEventLimit = 1000 // tracked objects fetched to be counted; a busier period is reported as "at least"
	digestNotable    = 3    // items listed under "Worth a look"
	digestMinScore   = 30   // an item that scores less isn't worth a look
	digestListed     = 5    // plates listed before "and N more"
	quietFrom        = 23   // overnight starts at 23:00 in the server's time zone...
	quietTo          = 6    // ...and ends at 06:00
)

type DigestInput struct {
	Camera string `json:"camera,omitempty" jsonschema:"only this camera; omit for all cameras"`
	Since  string `json:"since,omitempty" jsonschema:"how far back: e.g. 30m, 6h, 2d, 1w, today, yesterday. Default 24h. For a time of day use from and to instead"`
	From   string `json:"from,omitempty" jsonschema:"start of a time range, e.g. 11:30, 2pm, noon, yesterday 18:00. For \"overnight\" use from yesterday 22:00 and to 07:00"`
	To     string `json:"to,omitempty" jsonschema:"end of a time range, same forms as from. Default now"`
}

func (t *Toolset) dailyDigest(ctx context.Context, _ *mcp.CallToolRequest, in DigestInput) (*mcp.CallToolResult, any, error) {
	sel, err := t.selectReviews(ctx, "all", in.Camera, timeRange{in.Since, in.From, in.To})
	if err != nil {
		return nil, nil, err
	}
	evs, err := t.selectEvents(ctx, EventsInput{Camera: in.Camera, Since: in.Since, From: in.From, To: in.To}, digestEventLimit)
	if err != nil {
		return nil, nil, err
	}
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	cameras := cat.cameras
	if in.Camera != "" {
		name, err := cat.camera(in.Camera)
		if err != nil {
			return nil, nil, err
		}
		cameras = []string{name}
	}
	d := digest{period: sel.period, where: sel.where, now: sel.now, cameras: cameras,
		reviews: sel.shown, events: evs.events, truncated: evs.full()}
	slog.Info("daily_digest", "period", sel.period, "camera", in.Camera, "reviews", len(d.reviews), "events", len(d.events))
	return text(d.text()), nil, nil
}

// digest is everything a summary is made from. Items from cameras not in cameras are ignored.
type digest struct {
	period, where string
	now           time.Time
	cameras       []string // visible cameras to cover, sorted
	reviews       []frigate.Review
	events        []frigate.Event
	truncated     bool // the event query hit its limit, so counts are lower bounds
}

// camTally is what happened on one camera.
type camTally struct {
	alerts, detections int
	labels             map[string]int
}

func (c *camTally) total() int {
	n := c.alerts + c.detections
	for _, k := range c.labels {
		n += k
	}
	return n
}

func (d digest) text() string {
	loc := d.now.Location()
	tally := map[string]*camTally{}
	for _, c := range d.cameras {
		tally[c] = &camTally{labels: map[string]int{}}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Digest for %s, %s. Times are %s.\n", d.period, d.where, d.now.Format("MST"))

	var reviews []frigate.Review
	alerts, detections := 0, 0
	for _, r := range d.reviews {
		c, ok := tally[r.Camera]
		if !ok {
			continue
		}
		reviews = append(reviews, r)
		if r.Severity == "alert" {
			alerts++
			c.alerts++
		} else {
			detections++
			c.detections++
		}
	}
	labels, names, plates := map[string]int{}, map[string]int{}, map[string]int{}
	hours := map[time.Time]int{}
	events := 0
	for _, e := range d.events {
		c, ok := tally[e.Camera]
		if !ok {
			continue
		}
		events++
		labels[e.Label]++
		c.labels[e.Label]++
		st := e.Start.In(loc)
		hours[time.Date(st.Year(), st.Month(), st.Day(), st.Hour(), 0, 0, 0, loc)]++
		if e.SubLabel != "" {
			names[string(e.SubLabel)]++
		}
		if e.Data.Plate != "" {
			plates[e.Data.Plate]++
		}
	}
	if len(reviews) == 0 && events == 0 {
		b.WriteString("No alerts, detections or tracked objects.\n")
		return b.String()
	}

	fmt.Fprintf(&b, "Alerts: %d, detections: %d.\n", alerts, detections)
	if events == 0 {
		b.WriteString("Tracked objects: none.\n")
	} else {
		atLeast := ""
		if d.truncated {
			atLeast = "at least "
		}
		fmt.Fprintf(&b, "Tracked objects: %s%d (%s).\n", atLeast, events, countList(labels, false, 0))
	}

	if len(d.cameras) > 1 {
		order := append([]string(nil), d.cameras...)
		sort.SliceStable(order, func(i, j int) bool { return tally[order[i]].total() > tally[order[j]].total() })
		var lines, quiet []string
		for _, name := range order {
			ct := tally[name]
			if ct.total() == 0 {
				quiet = append(quiet, name)
				continue
			}
			var parts []string
			if ct.alerts > 0 {
				parts = append(parts, plural(ct.alerts, "alert"))
			}
			if ct.detections > 0 {
				parts = append(parts, plural(ct.detections, "detection"))
			}
			line := "- " + name + ": " + strings.Join(parts, ", ")
			if len(ct.labels) > 0 {
				if len(parts) > 0 {
					line += "; "
				}
				line += "tracked: " + countList(ct.labels, false, 0)
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			b.WriteString("By camera, busiest first:\n" + strings.Join(lines, "\n") + "\n")
		}
		if len(quiet) > 0 {
			fmt.Fprintf(&b, "No activity: %s.\n", strings.Join(quiet, ", "))
		}
	}

	var busiest time.Time
	most := 0
	for h, n := range hours {
		if n > most || (n == most && h.Before(busiest)) {
			busiest, most = h, n
		}
	}
	if most >= 2 {
		fmt.Fprintf(&b, "Busiest hour: %s-%s (%s).\n", clock(busiest, d.now), busiest.Add(time.Hour).Format("15:04"), plural(most, "object"))
	}
	if len(names) > 0 {
		fmt.Fprintf(&b, "Recognized: %s.\n", countList(names, true, digestListed))
	}
	if len(plates) > 0 {
		fmt.Fprintf(&b, "Plates: %s.\n", countList(plates, true, digestListed))
	}

	if notes := notable(reviews, loc); len(notes) == 0 {
		b.WriteString("Nothing stood out: no overnight alerts, and nothing Frigate rated suspicious or critical.\n")
	} else {
		b.WriteString("Worth a look, most notable first:\n")
		for _, n := range notes {
			b.WriteString(reviewLine(n.r, true, d.now))
			fmt.Fprintf(&b, "  Why: %s.\n", strings.Join(n.why, "; "))
		}
		b.WriteString("Use review_alert with an id to check whether one was real, or get_alert_snapshot to see it.\n")
	}
	b.WriteString("For the full lists, use get_alerts and get_events.\n")
	return b.String()
}

// noted is a review item worth a second look, and why.
type noted struct {
	r     frigate.Review
	score int
	why   []string
}

// notable picks the items most worth a look: alerts overnight, and anything Frigate's GenAI
// review rated suspicious or critical. Everything else is routine and only counted.
func notable(reviews []frigate.Review, loc *time.Location) []noted {
	var out []noted
	for _, r := range reviews {
		n := noted{r: r}
		if r.Severity == "alert" {
			n.score += 10
			if overnight(r.Start.In(loc)) {
				n.score += 30
				n.why = append(n.why, "overnight")
			}
		}
		if m := r.Data.Metadata; m != nil && m.ThreatLevel > 0 {
			n.score += 50 * m.ThreatLevel
			level := "suspicious"
			if m.ThreatLevel >= 2 {
				level = "critical"
			}
			n.why = append(n.why, "Frigate rated it "+level)
		}
		if n.score >= digestMinScore {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].r.Start.After(out[j].r.Start.Time)
	})
	return out[:min(len(out), digestNotable)]
}

func overnight(t time.Time) bool {
	h := t.Hour()
	return h >= quietFrom || h < quietTo
}

// ranked lists the keys of m, most first, ties by name.
func ranked(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

// countList writes counts most first: "car 15, person 12", or "Alex (2)" with paren. A limit above
// zero cuts the list there and says how many were left out.
func countList(m map[string]int, paren bool, limit int) string {
	keys := ranked(m)
	more := 0
	if limit > 0 && len(keys) > limit {
		more = len(keys) - limit
		keys = keys[:limit]
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		if paren {
			parts[i] = fmt.Sprintf("%s (%d)", k, m[k])
		} else {
			parts[i] = fmt.Sprintf("%s %d", k, m[k])
		}
	}
	s := strings.Join(parts, ", ")
	if more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}
