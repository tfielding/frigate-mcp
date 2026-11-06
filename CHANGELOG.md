# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- `-healthcheck` flag for Docker's `healthcheck:`: checks the running server's `/healthz` from inside the
  container (the image has no shell or curl) and exits 0 when healthy. Lets other services use
  `depends_on` with `condition: service_healthy`.
- `get_event_frames`: an object at up to four moments of its visit (first seen, entering a zone, while
  there, last seen) from the recordings, each zoomed in on it, in one picture. Shows whether something
  walked up or just appeared in place.
- `get_event_timeline` starts with a line of facts per object, to help tell real objects from false
  detections: Frigate's top confidence, whether it came into its zone from outside or appeared inside
  it, and whether it moved.

## [0.1.0] - 2026-11-06

### Added
- Read-only MCP server (Streamable HTTP) for Frigate NVR 0.18.
- `get_alerts`: counts of alerts and detections over a time range, and a list of them (newest first)
  with camera, objects, sub-labels, zones and audio, plus Frigate's GenAI description when present.
- `get_alert_snapshot`: the recorded frame of an alert or detection, falling back to an object snapshot.
- `get_alerts_grid`: the newest alerts (up to 9) as one numbered picture grid with a matching list, for
  local vision models that handle one image better than several. Tiles show the number, time and objects;
  `GRID_CAMERA_LABELS=true` adds the camera name. Tiles use object snapshots with boxes, falling back to
  recorded frames (`GRID_BOXES=false` for recorded frames first).
- `GRID_LABEL_POSITION`: which corner grid tile labels go in (default bottom-left, clear of Frigate's
  timestamp).
- Grid tiles zoom in on objects that are small in the frame, so distant people and cars are
  recognizable (`GRID_CROP`: `auto` by default, `always` or `off`).
- `get_camera_frame`: a camera now, or at a past time from the recordings, or two times side by
  side (`compare`), e.g. "has the package been taken?". Says when a camera seems offline, since
  Frigate's latest picture is then an older one.
- `get_alert_clip`: Frigate's animated preview (GIF) of an alert or detection, from a few seconds
  before to a few seconds after.
- `last_seen`: when an object, or a recognized person or plate (`name`), was last seen, in one line
  with how long ago.
- `get_event_timeline`: what a tracked object (or each object of an alert) did, from Frigate's
  timeline, as text: when it appeared and where in the frame, zones entered, when it stood still
  and for how long, faces or plates seen, and when it left.
- `get_system_status`: cameras that are offline, poorly connected or skipping frames, detector speed,
  free disk space and the Frigate version (with any newer release), from `/api/stats`. Works with
  the viewer role.
- `get_events_grid` and `search_events_grid` (with semantic search): the same picture grid for tracked
  objects, using their snapshots with boxes.
- `get_events` and `get_event_snapshot`: tracked objects filtered by camera, label, zone and time.
- `from` and `to` on every list, grid and search tool, for a time of day ("around noon": `from: 11:30,
  to: 12:30`); lists cut off at their limit end with the exact `to` for fetching older items. A `to`
  without a day is read relative to `from`, so ranges asked about before they end, or overnight,
  work. `get_events` says how many it found.
- `search_events`: semantic search, offered only while semantic search is enabled in Frigate.
- Login with a Frigate user (viewer role is enough) and automatic re-login, or the internal port
  without login. Optional camera allowlist, bearer token, snapshot height, time zone, `/healthz`.
- `-version` flag.
