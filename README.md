# frigate-mcp

[![ci](https://github.com/tfielding/frigate-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/tfielding/frigate-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A small, read-only [MCP](https://modelcontextprotocol.io) server that lets an AI assistant answer
questions about what your cameras saw, using [Frigate](https://frigate.video) NVR: "anything happen
today?", "show me that one", "when was the last car in the driveway?".

```
MCP client (e.g. signal-ollama) ⇄ frigate-mcp ⇄ Frigate
        POST /mcp (Streamable HTTP)     /api/review, /api/events, snapshots
```

Tested against Frigate 0.18.

## Tools

| Tool | Input | Returns |
|---|---|---|
| `get_alerts` | optional `severity` (`alert` default, `detection`, `all`), `camera`, `since`, `from`, `to`, `limit` | Counts of alerts and detections, then a list, newest first |
| `get_alerts_grid` | optional `severity`, `camera`, `since`, `from`, `to`, `limit` (default 6, at most 9) | One numbered grid picture of the newest items (JPEG), plus the same list as `get_alerts` with matching numbers |
| `get_alert_snapshot` | `id` from `get_alerts` | The picture for that item (JPEG) |
| `get_alert_clip` | `id` from `get_alerts` | A short animated clip of that item (GIF), from a few seconds before to a few seconds after |
| `get_events` | optional `camera`, `label`, `zone`, `since`, `from`, `to`, `limit` | Tracked objects, newest first |
| `get_events_grid` | same as `get_events`, `limit` default 6, at most 9 | One numbered grid of the objects' snapshots with boxes (JPEG), plus the same list as `get_events` |
| `get_event_snapshot` | `id` from `get_events` / `search_events` | The object's saved snapshot with its box drawn (JPEG) |
| `search_events` | `query`, optional `camera`, `since`, `from`, `to`, `limit` | Best matches by appearance. **Only offered while semantic search is enabled in Frigate** |
| `search_events_grid` | same as `search_events`, `limit` default 6, at most 9 | One numbered grid of the best matches' snapshots with boxes (JPEG), plus the list. **Only with semantic search, like `search_events`** |
| `get_system_status` | none | Whether Frigate and the cameras are working: offline or poorly connected cameras, detector speed, free disk space, Frigate version |
| `get_camera_frame` | `camera`, optional `at` (default now), optional `compare` | What the camera sees now or saw at a past time (JPEG); with `compare`, two times side by side in one picture |
| `last_seen` | `label` and/or `name`, optional `camera`, `zone`, `within` | When that object or recognized person/plate was last seen, in one line |
| `get_event_timeline` | `id` from `get_events`, `last_seen` or `get_alerts` | What the object did, step by step, as text: appeared, entered zones, stood still (how long), left; with its top confidence and whether it moved |
| `get_event_frames` | `id` from `get_events`, `last_seen` or `get_alerts` | The object at up to 4 moments of its visit (first seen, entered a zone, while there, last seen), zoomed in, in one picture |
| `review_alert` | `id` from `get_alerts`, `get_events` or `last_seen` | Both of the above in one call: the timeline with its facts line, and the frames picture. For checking whether an alert was real |

`since` takes `30m`, `6h`, `2d`, `1w`, `today` or `yesterday` (default 24 hours, at most 31 days).
For a time of day, `from` and `to` take times the way people say them: `11:30`, `2pm`, `noon`,
`yesterday 18:00`, `2026-10-30 09:15`. "Cars around noon" is `from: 11:30, to: 12:30`. A `from`
without a day is the most recent one, so at 10:00 `16:00` means yesterday; a `to` without a day is
the first such time after `from` (`from: 23:00, to: 06:00` runs overnight), and the range never
ends later than now, so "around noon" asked at 12:10 covers 11:30 until then. `to` alone ends the
range there (with `since`, or 24 hours, counted back from it).

Lists are newest first and stop at `limit`. When they do, the answer ends with the exact way to get
the earlier ones, e.g. `For older ones, call again with the same arguments and to: 12:28:40`, so a
model can page back through a busy day.
Camera, label and zone names are checked against Frigate's config and matched loosely (`Front Door`
finds `front_door`, `cars` finds `car`); a wrong name returns the valid ones so the model can retry.

Example `get_alerts` output:

```
Last 24 hours, all cameras: 3 alerts, 12 detections. Times are EST.
Alerts (newest first):
- today 14:12 (40s), front_door: person, car; zones: porch, street; id 1761925920.5-ab12cd
- today 02:14 (1m05s), driveway: person; zones: driveway; id 1761884040.1-ef34gh
- yesterday 18:30 (12s), backyard: dog; id 1761863400.9-ij56kl
Use get_alert_snapshot with an id to show the picture.
```

`get_alert_snapshot` returns the recorded frame Frigate uses as the item's thumbnail; if recordings
are off it falls back to the snapshot of an object in it.

`get_alert_clip` returns Frigate's preview of the item as an animated GIF, from 8 seconds before it
started to 8 seconds after it ended, so you see what happened rather than one moment. It's made from
Frigate's low-resolution preview recordings, so it's small; if they don't cover the item, the answer
says so. Vision models only look at still pictures, so the clip is for you (MCP clients that
forward images, like signal-ollama, send it as an attachment).

`get_alerts_grid` puts those pictures for several items into one image, each tile labelled with its
number, time and what was seen (e.g. `3) 14:12 (person, car)`), and the camera name too with
`GRID_CAMERA_LABELS=true`:

```
Last 24 hours, all cameras: 3 alerts, 12 detections. Times are EST.
Pictures numbered as in the grid (left to right, top to bottom), newest first:
1. today 14:12 (40s), front_door: person, car; zones: porch, street; id 1761925920.5-ab12cd
2. today 02:14 (1m05s), driveway: person; zones: driveway; id 1761884040.1-ef34gh
3. yesterday 18:30 (12s), backyard: dog; id 1761863400.9-ij56kl
Use get_alert_snapshot with an id for a bigger picture of one.
```

It's meant for local vision models, many of which handle one image much better than several, and
for keeping context small: "show me today's alerts" costs one picture instead of one per alert. The
grid fits within `SNAPSHOT_HEIGHT` (at least 360 pixels tall, 1080 when `SNAPSHOT_HEIGHT=0`) at 16:9,
and each picture is fetched from Frigate at its tile size. By default each tile is the snapshot of one
of the item's objects with its box drawn by Frigate (so with several objects, one is boxed), falling
back to the recorded frame when the camera has no snapshots; `GRID_BOXES=false` uses the recorded
frame first, like `get_alert_snapshot`.

`get_events_grid` and `search_events_grid` work the same way for tracked objects ("show me all the
cars today", "show me red trucks"), always using the objects' snapshots with boxes. `get_events_grid`
needs only regular object detection; objects without a saved snapshot get an empty tile. Pictures keep their shape, so portrait
doorbells and panoramic cameras are letterboxed. An item with no picture gets an empty tile.

Tiles zoom in on an object that's small in the frame (its larger side under 40% of it), so a person
at the end of the driveway is recognizable instead of a few pixels; a close object keeps the whole
scene around it. Frigate does the cropping (`crop=1` on the snapshot). `GRID_CROP=always` zooms in on
every object, `off` never does. Recorded frames and the single-picture tools are never cropped.

`get_camera_frame` answers "is the car still in the driveway?" (now) and "what did the porch look
like at 3pm?" (from the recordings), and with `compare` shows two times side by side: "has the
package been taken?" is `at: 10:15, compare: now`. Times take the same forms as `from`/`to`. A past
time needs a recording then; Frigate may only keep recordings around events, and says so. If a
camera is offline right now, Frigate's "latest" picture is an older frame or a placeholder, so the
answer says the camera seems offline.

`last_seen` answers "when was the last car in the driveway?" in one line, with how long ago and for
how long (or that it's there now), plus the id for `get_event_snapshot`:

```
Last car on driveway: today 14:00 (1 hour ago), for 6m00s. Zones: driveway. Id 1762270000.1-abc.
```

`name` looks for a recognized face or plate (Frigate's face or license plate recognition), matched
loosely against the names Frigate knows; it searches the last 31 days unless `within` says less.

`get_event_timeline` says what a tracked object did, from Frigate's timeline, as text, e.g. "did
they come to the door or just walk past?". It takes an id from `get_events` or `last_seen`, or an
alert's id from `get_alerts` (its objects, up to 5):

```
Timeline of person on front_door, today 14:12 (55s):
Frigate's top confidence 86%; first seen outside its zones, entered porch after 5s; moved across the frame.
- 14:12:03 person seen, near the left edge
- 14:12:08 entered porch
- 14:12:13 stopped moving
- 14:12:23 face visible
- 14:12:53 moving again (still for 40s)
- 14:12:58 gone, last seen near the right edge
```

The first line sums up each object, to help tell a real one from a false detection: Frigate's top
confidence (its best score over the object's whole life, so a person who scores low once they're
close to a doorbell camera still shows the higher score from their approach), whether it came into
its zone from outside or appeared already inside it, and whether it moved. A low score, appearing
inside a zone and staying in one place together suggest a false detection.

Frigate keeps the timeline only as long as the tracked object, so older ones may have none.

`get_event_frames` shows the same visit as pictures: up to four moments (first seen, entering a
zone, while there, last seen) from the recordings, each zoomed in on the object, numbered in one
image. A real visitor is seen walking up; a false detection tends to sit in the same spot in every
frame, which a single snapshot can't show. It needs recordings, and uses an alert's first object.

`review_alert` returns both in one call: the timeline text followed by the frames picture. It's
meant for automated alert checks, where every round trip to the model adds seconds: one tool call
instead of two or more. Without recordings, it still returns the timeline and says why there's no
picture.

If [GenAI review descriptions](https://docs.frigate.video/configuration/genai/genai_review) are configured in
Frigate, each item also gets Frigate's own title, summary and threat level. Without them, the list
says what was detected but not what happened.

Alerts vs detections, and which objects count as alerts, are Frigate's
[review settings](https://docs.frigate.video/configuration/review). Frigate tracks "reviewed" per
user, so frigate-mcp doesn't report it: everything would look unreviewed to its own user.

All tools are marked read-only. Nothing can be marked reviewed, deleted or changed.

## Frigate access

Use one of Frigate's two API ports:

- **8971, with a login (recommended).** Add a user with the **viewer** role in Frigate
  (Settings → Users) and set `FRIGATE_USERNAME` / `FRIGATE_PASSWORD`. frigate-mcp logs in and logs
  in again when the session expires. Frigate uses a self-signed certificate on this port unless you
  install your own, so set `FRIGATE_TLS_INSECURE=true` in that case.
- **5000, no login.** Frigate treats every request on this port as an admin, so only use it on a
  private Docker network. Leave the username and password empty.

## Safety

- **Read-only, least privilege.** A viewer user is enough; frigate-mcp only reads.
- **Known names only.** Cameras, labels and zones must exist in Frigate's config; ids are looked up
  before any image is fetched.
- **Optional camera allowlist** (`CAMERAS`) to expose only some cameras. Items from other cameras
  are never listed or shown, even by id.
- **No internal details to the client.** Frigate errors are logged; the client gets a short message.
- **Optional bearer token** (`AUTH_TOKEN`) for MCP clients.
- Runs as a non-root user in a minimal image with no shell.

## Configuration

| Variable | Default | |
|---|---|---|
| `FRIGATE_URL` | required | e.g. `https://frigate:8971` or `http://frigate:5000` |
| `FRIGATE_USERNAME` / `FRIGATE_PASSWORD` | none | Frigate user for port 8971 (viewer role is enough) |
| `FRIGATE_TLS_INSECURE` | `false` | Accept Frigate's self-signed certificate |
| `FRIGATE_TIMEOUT` | `20s` | Limit for each Frigate request |
| `CAMERAS` | all | Comma-separated allowlist (case-insensitive) |
| `SNAPSHOT_HEIGHT` | `720` | Image height in pixels; `0` = native. Also the height `get_alerts_grid` fits in |
| `GRID_BOXES` | `true` | `get_alerts_grid` shows object snapshots with boxes; `false` for plain recorded frames |
| `GRID_CROP` | `auto` | When grid tiles zoom in on the object: `auto` (when it's small in the frame), `always` or `off` |
| `GRID_LABEL_POSITION` | `bottom-left` | Corner for grid tile labels: `top-left`, `top-right`, `bottom-left` or `bottom-right`. Pick one clear of Frigate's timestamp and your cameras' own on-screen text |
| `GRID_CAMERA_LABELS` | `false` | Also write the camera name on `get_alerts_grid` tiles |
| `TZ` | UTC | Time zone for times shown to the model, e.g. `America/New_York` |
| `AUTH_TOKEN` | none | Require `Authorization: Bearer <token>` on `/mcp` |
| `LISTEN_ADDR` | `:8091` | |

## Run

```yaml
services:
  frigate-mcp:
    image: ghcr.io/tfielding/frigate-mcp:latest
    restart: always
    healthcheck:
      test: ["CMD", "/frigate-mcp", "-healthcheck"]
      interval: 1m
      timeout: 15s
      start_period: 30s
    environment:
      - FRIGATE_URL=https://<frigate-host>:8971
      - FRIGATE_USERNAME=mcp
      - FRIGATE_PASSWORD=...
      - FRIGATE_TLS_INSECURE=true
      - TZ=America/New_York
```

MCP clients on the same Docker network connect to `http://frigate-mcp:8091/mcp`.
`GET /healthz` returns 200 when Frigate is reachable and the login works. The image has no shell or
curl, so the `healthcheck:` above runs `frigate-mcp -healthcheck`, which checks `/healthz` from inside
the container. With it, other services can wait for frigate-mcp with
`depends_on: {frigate-mcp: {condition: service_healthy}}`.

Quick check that the user works, before involving MCP at all:

```bash
curl -k -c /tmp/f -H 'Content-Type: application/json' \
  -d '{"user":"mcp","password":"..."}' https://<frigate-host>:8971/api/login
curl -k -b /tmp/f "https://<frigate-host>:8971/api/review?severity=alert" | head -c 300
```

Locally:

```bash
FRIGATE_URL=http://<frigate-host>:5000 go run .
```

## Install without Docker

```bash
go install github.com/tfielding/frigate-mcp@latest
frigate-mcp -version
```

## Develop

```bash
go test -race ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full set of checks CI runs, and
[SECURITY.md](SECURITY.md) for reporting vulnerabilities. Changes are listed in
[CHANGELOG.md](CHANGELOG.md).

Tests run the real MCP client from the official Go SDK against the server and a fake Frigate.

## License

MIT. Third-party licenses are in `THIRD_PARTY_LICENSES` (also copied into the image at `/licenses`).

frigate-mcp is an independent project, not affiliated with or endorsed by the Frigate project.
