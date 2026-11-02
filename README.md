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
| `get_alerts` | optional `severity` (`alert` default, `detection`, `all`), `camera`, `since`, `limit` | Counts of alerts and detections, then a list, newest first |
| `get_alert_snapshot` | `id` from `get_alerts` | The picture for that item (JPEG) |
| `list_events` | optional `camera`, `label`, `zone`, `since`, `limit` | Tracked objects, newest first |
| `get_event_snapshot` | `id` from `list_events` / `search_events` | The object's saved snapshot with its box drawn (JPEG) |
| `search_events` | `query`, optional `camera`, `since`, `limit` | Best matches by appearance. **Only offered while semantic search is enabled in Frigate** |

`since` takes `30m`, `6h`, `2d`, `1w`, `today` or `yesterday` (default 24 hours, at most 31 days).
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
| `SNAPSHOT_HEIGHT` | `720` | Image height in pixels; `0` = native |
| `TZ` | UTC | Time zone for times shown to the model, e.g. `America/New_York` |
| `AUTH_TOKEN` | none | Require `Authorization: Bearer <token>` on `/mcp` |
| `LISTEN_ADDR` | `:8091` | |

## Run

```yaml
services:
  frigate-mcp:
    image: ghcr.io/tfielding/frigate-mcp:latest
    restart: always
    environment:
      - FRIGATE_URL=https://<frigate-host>:8971
      - FRIGATE_USERNAME=mcp
      - FRIGATE_PASSWORD=...
      - FRIGATE_TLS_INSECURE=true
      - TZ=America/New_York
```

MCP clients on the same Docker network connect to `http://frigate-mcp:8091/mcp`.
`GET /healthz` returns 200 when Frigate is reachable and the login works.

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
