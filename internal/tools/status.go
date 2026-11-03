package tools

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tfielding/frigate-mcp/internal/frigate"
)

const (
	slowInference = 100.0 // ms per detection above which Frigate's own UI calls a detector slow
	lowDiskShare  = 0.10  // free share of a disk below which it's reported as low
)

// Frigate's storage paths, and how to name them.
var storagePaths = []struct{ path, name string }{
	{"/media/frigate/recordings", "recordings"},
	{"/media/frigate/clips", "snapshots and clips"},
}

type StatusInput struct{}

// systemStatus reports whether Frigate and the cameras are working, problems first.
func (t *Toolset) systemStatus(ctx context.Context, _ *mcp.CallToolRequest, _ StatusInput) (*mcp.CallToolResult, any, error) {
	cat, err := t.cat.get(ctx)
	if err != nil {
		return nil, nil, unavailable(err)
	}
	st, err := t.src.Stats(ctx)
	if err != nil {
		slog.Error("get_system_status", "err", err)
		return nil, nil, unavailable(err)
	}
	now := t.opt.Now().In(t.opt.Location)
	var b strings.Builder

	fmt.Fprintf(&b, "Frigate %s", baseVersion(st.Service.Version))
	if newer(st.Service.LatestVersion, st.Service.Version) {
		fmt.Fprintf(&b, " (version %s is available)", st.Service.LatestVersion)
	}
	if st.Service.Uptime > 0 {
		fmt.Fprintf(&b, ", running for %s", uptime(time.Duration(st.Service.Uptime)*time.Second))
	}
	fmt.Fprintf(&b, ". Checked %s.\n", now.Format("15:04 MST"))

	problems, ok := cameraStatus(cat.cameras, st.Cameras)
	switch {
	case len(problems) == 0 && len(ok) == 1:
		fmt.Fprintf(&b, "Camera: %s is OK.\n", ok[0])
	case len(problems) == 0:
		fmt.Fprintf(&b, "Cameras: all %d OK (%s).\n", len(ok), strings.Join(ok, ", "))
	default:
		fmt.Fprintf(&b, "Cameras: %d of %d OK.\n", len(ok), len(ok)+len(problems))
		for _, p := range problems {
			fmt.Fprintf(&b, "- %s\n", p)
		}
		if len(ok) > 0 {
			fmt.Fprintf(&b, "- OK: %s\n", strings.Join(ok, ", "))
		}
	}

	for _, line := range detectorStatus(st) {
		b.WriteString(line + "\n")
	}
	for _, line := range storageStatus(st.Service.Storage) {
		b.WriteString(line + "\n")
	}
	slog.Info("get_system_status", "camera_problems", len(problems), "cameras_ok", len(ok))
	return text(b.String()), nil, nil
}

// cameraStatus describes each visible camera: problems (one line each) and the names of those
// that are fine. Cameras are visible ones from the catalog; Frigate's stats may lack some.
func cameraStatus(cameras []string, stats map[string]frigate.CameraStats) (problems, ok []string) {
	for _, name := range cameras {
		s, found := stats[name]
		if !found {
			problems = append(problems, name+": no status from Frigate")
			continue
		}
		var issues []string
		fps := fmt.Sprintf("%s fps", trimFloat(s.CameraFPS))
		if s.ExpectedFPS > 0 {
			fps = fmt.Sprintf("%s of %s fps", trimFloat(s.CameraFPS), trimFloat(s.ExpectedFPS))
		}
		switch {
		case s.CameraFPS < 0.1:
			issues = append(issues, "OFFLINE (no video)")
		case s.ConnectionQuality == "unusable":
			issues = append(issues, "very poor connection ("+fps+")")
		case s.ConnectionQuality == "poor":
			issues = append(issues, "poor connection ("+fps+")")
		}
		if len(issues) > 0 && s.ReconnectsLastHour > 0 {
			issues = append(issues, plural(s.ReconnectsLastHour, "reconnect")+" in the last hour")
		}
		if s.CameraFPS >= 0.1 && s.SkippedFPS >= 1 {
			issues = append(issues, fmt.Sprintf("skipping %s frames/s (the detector can't keep up)", trimFloat(s.SkippedFPS)))
		}
		switch {
		case len(issues) > 0:
			if !s.DetectionEnabled {
				issues = append(issues, "object detection is off")
			}
			problems = append(problems, name+": "+strings.Join(issues, "; "))
		case !s.DetectionEnabled: // often on purpose, e.g. indoors: not a fault
			ok = append(ok, name+" (object detection off)")
		default:
			ok = append(ok, name)
		}
	}
	return problems, ok
}

func detectorStatus(st *frigate.Stats) []string {
	names := make([]string, 0, len(st.Detectors))
	for n := range st.Detectors {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []string
	for _, n := range names {
		d := st.Detectors[n]
		line := fmt.Sprintf("Detector %s: %s ms per detection", n, trimFloat(d.InferenceSpeed))
		if d.InferenceSpeed > slowInference {
			line += " (slow)"
		}
		if d.Temperature != nil {
			line += fmt.Sprintf(", %s°C", trimFloat(*d.Temperature))
		}
		out = append(out, line+".")
	}
	return out
}

// storageStatus reports free space on Frigate's disks; paths on the same disk are reported once.
func storageStatus(storage map[string]frigate.StorageStats) []string {
	type disk struct {
		names []string
		s     frigate.StorageStats
	}
	var disks []*disk
	for _, p := range storagePaths {
		s, found := storage[p.path]
		if !found || s.Total <= 0 {
			continue
		}
		var d *disk
		for _, existing := range disks {
			if existing.s == s {
				d = existing
			}
		}
		if d == nil {
			d = &disk{s: s}
			disks = append(disks, d)
		}
		d.names = append(d.names, p.name)
	}
	var out []string
	for _, d := range disks {
		line := fmt.Sprintf("Storage for %s: %s free of %s (%d%% used)", strings.Join(d.names, " and "),
			size(d.s.Free), size(d.s.Total), int(d.s.Used/d.s.Total*100+0.5))
		if d.s.Free < d.s.Total*lowDiskShare {
			line += "; low on space, Frigate deletes the oldest recordings to make room"
		}
		out = append(out, line+".")
	}
	return out
}

// size formats a size in MiB, as Frigate reports it.
func size(mib float64) string {
	switch {
	case mib >= 1<<20:
		return fmt.Sprintf("%.1f TB", mib/(1<<20))
	case mib >= 1<<10:
		return fmt.Sprintf("%.1f GB", mib/(1<<10))
	}
	return fmt.Sprintf("%.0f MB", mib)
}

func uptime(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	switch {
	case days > 0:
		return plural(days, "day") + " " + plural(hours, "hour")
	case hours > 0:
		return plural(hours, "hour") + " " + plural(int(d/time.Minute)%60, "minute")
	}
	return plural(int(d/time.Minute), "minute")
}

func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// baseVersion drops the build suffix: "0.18.2-6ef3a7b" becomes "0.18.2".
func baseVersion(v string) string {
	base, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-")
	return base
}

// newer reports whether version latest is newer than current, comparing dotted numbers.
// Frigate reports "unknown" or "disabled" when it hasn't checked.
func newer(latest, current string) bool {
	l, c := strings.Split(baseVersion(latest), "."), strings.Split(baseVersion(current), ".")
	for i := range max(len(l), len(c)) {
		var a, b int
		var err error
		if i < len(l) {
			if a, err = strconv.Atoi(l[i]); err != nil {
				return false
			}
		}
		if i < len(c) {
			if b, err = strconv.Atoi(c[i]); err != nil {
				return false
			}
		}
		if a != b {
			return a > b
		}
	}
	return false
}
