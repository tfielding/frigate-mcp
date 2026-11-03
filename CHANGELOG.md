# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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
