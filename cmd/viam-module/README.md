# viam-module

The [Viam](https://www.viam.com/) module entry point. It registers two models:

- `cheukt:wyze-bridge:manager` — the generic service documented below.
- `cheukt:wyze-bridge:conditional-camera` — a camera that gates recordings on
  recent motion events (see [below](#conditional-camera-component)).

The LAN-local dashboard that used to live here (`cat-ui`) now ships as a
separate module, [`cheukt:home:home-ui`](https://github.com/cheukt/home).

The `manager` service embeds the [wyze-headless](../wyze-headless/) core — Wyze
auth + camera discovery + an embedded go2rtc — in-process and publishes each
camera as a loopback RTSP stream that a standard `viam:viamrtsp:rtsp` camera
component consumes.

Wyze credentials are read from an on-machine dotenv file referenced by
`creds_file`, so secrets never enter the Viam cloud config. See
[DOCS/VIAM_MODULE.md](../../DOCS/VIAM_MODULE.md) for the full design.

## Configuration

The service is registered as `rdk:service:generic` / model
`cheukt:wyze-bridge:manager`. Its (non-secret) attributes:

| Attribute | Required | Default | Purpose |
|---|---|---|---|
| `creds_file` | ✅ | — | Path on the machine to the dotenv credentials file (mode `0600`) holding `WYZE_EMAIL`/`WYZE_PASSWORD`/`WYZE_API_ID`/`WYZE_API_KEY` |
| `bridge_ip` | — | — | Host IP advertised in WebRTC ICE candidates |
| `state_dir` | — | `$VIAM_MODULE_DATA` (else `./local/config`) | Where `go2rtc.yaml` + Wyze state are written |
| `rtsp_port` | — | `8554` | go2rtc RTSP listen port; reflected in the `rtsp_url`s reported by `list_cameras` |
| `log_level` | — | `info` | `trace`/`debug`/`info`/`warn`/`error` |
| `force_iotc_detail` | — | `false` | Verbose go2rtc + bridge logging |
| `stun_server` | — | `stun:stun.l.google.com:19302` | WebRTC STUN server override |
| `filter_names` | — | — | Restrict which cameras are exposed, by nickname (case-insensitive). Empty = expose all |
| `filter_models` | — | — | Restrict by model code or human-readable model name (case-insensitive) |
| `filter_macs` | — | — | Restrict by MAC address (case-insensitive) |
| `filter_block` | — | `false` | Invert the filters: matched cameras are *excluded* instead of being the only ones included |

### Restricting which cameras are exposed

By default every discovered camera is published as a stream. Set one or more of
`filter_names` / `filter_models` / `filter_macs` to expose only a subset — a
camera matching **any** of them is selected. With `filter_block: true` the sense
inverts and matched cameras are excluded.

Unlike the Docker bridge's `FILTER_*` env vars (which fall back to all cameras if
a filter matches nothing), the filter here is treated as an intentional
allow-list: a filter that matches **no** cameras exposes **nothing**.

Example service config:

```json
{
  "name": "wyze",
  "api": "rdk:service:generic",
  "model": "cheukt:wyze-bridge:manager",
  "attributes": {
    "creds_file": "/etc/wyze-bridge/creds.env",
    "rtsp_port": 8554,
    "filter_names": ["Front Door", "Garage"]
  }
}
```

## DoCommand

`DoCommand` is the discovery + control surface. Each command is a single
top-level key; the value is either a bare `true` or an options object.

### `list_cameras` — cameras, state, and RTSP URLs

By default it **actively probes** each camera (fetches a frame from go2rtc,
which forces the lazy source to dial the camera) so readiness reflects reality:
`rtsp_url` is only present for cameras that actually produce media, and an
`error` reason is attached to those that don't.

```json
{ "list_cameras": true }
```

```json
{
  "cameras": [
    {
      "name": "front_door",
      "nickname": "Front Door",
      "model": "WYZE_CAKP2JFUS",
      "state": "streaming",
      "ready": true,
      "rtsp_url": "rtsp://127.0.0.1:8554/front_door"
    },
    {
      "name": "garage",
      "nickname": "Garage",
      "model": "HL_CAM4",
      "state": "error",
      "ready": false,
      "error": "discovery timeout"
    }
  ]
}
```

Pass `{"probe": false}` for a fast, side-effect-free list that trusts the
manager's (optimistic) state and always emits `rtsp_url`:

```json
{ "list_cameras": { "probe": false } }
```

### `get_events` — recent events

Fetches recent events (the Wyze alarm/motion feed) across all known cameras
within a lookback window. The camera is resolved from the event's MAC —
`device_id`, falling back to the `event_id` prefix (Wyze event ids are
`<MAC><timestamp>`) — back to the friendly camera name. The window defaults to
**5 minutes**; override it with `window_seconds`.

```json
{ "get_events": true }
```

```json
{ "get_events": { "window_seconds": 120 } }
```

```json
{
  "window_seconds": 120,
  "events": [
    {
      "camera": "front_door",
      "nickname": "Front Door",
      "mac": "80482CAA9F2F",
      "model": "HL_CAM4",
      "time": "2026-06-26T21:39:52Z",
      "event_ts": 1782509992799,
      "value": "1",
      "event_id": "80482CAA9F2F011782509992",
      "thumbnail_url": "https://prod-sight-safe-auth.wyze.com/resource/.../95fd...jpg?st=..."
    }
  ]
}
```

`thumbnail_url` (and `video_url`, when present) are signed, time-limited URLs
pulled from the event's `file_list` — fetch them promptly. `tags` carries
Wyze's AI detection labels (e.g. `["person"]`) and appears only when those fire,
which requires Cam Plus; plain motion events omit it.

### `restart_camera` — reconnect a stream

```json
{ "restart_camera": "front_door" }
```

```json
{ "restarted": "front_door" }
```

### `set_quality` — change a camera's quality

```json
{ "set_quality": { "name": "front_door", "quality": "hd" } }
```

```json
{ "name": "front_door", "quality": "hd" }
```

## Wiring a camera

1. Add this service to your machine config with a valid `creds_file`.
2. Call `list_cameras` to get the stream names + `rtsp_url`s.
3. Add a `viam:viamrtsp:rtsp` camera component pointing at the `rtsp_url`
   for each stream you want.

## `conditional-camera` component

The module also registers `rdk:component:camera` / model
`cheukt:wyze-bridge:conditional-camera`. It wraps an underlying camera (the
`viam:viamrtsp:rtsp` camera fed by this module) and only lets **data-management
captures** through when there was a recent Wyze motion event; live views are
never gated. Use it as the `camera` of a data-capture service to record only
around motion.

It answers the condition without hitting the Wyze API on every frame: a
background loop polls the manager's `get_events` once a second and caches the
newest matching event's timestamp. Two skips keep that cheap — while an event
is inside `window_seconds` the answer is already "yes", and Wyze can't emit a
new event during its per-camera `cooldown_seconds`, so the loop makes **no**
`get_events` calls from an event until the cooldown elapses, then resumes.

| Attribute | Required | Default | Purpose |
|---|---|---|---|
| `camera` | ✅ | — | Underlying camera resource name to gate (e.g. the `viamrtsp` camera) |
| `manager` | ✅ | — | Name of the `cheukt:wyze-bridge:manager` service to poll for events |
| `camera_name` | — | — | Only events whose `camera`/`nickname` match this (case-insensitive) count; empty = any camera's events trigger |
| `window_seconds` | — | `20` | How recent an event must be for frames to pass through |
| `cooldown_seconds` | — | `300` | Wyze's per-camera event cooldown; the poll loop stays quiet for this long after an event |
| `poll_seconds` | — | `1` | Background poll cadence |
| `debug` | — | `false` | Verbose per-poll logging |

Example component config:

```json
{
  "name": "front_door_motion",
  "api": "rdk:component:camera",
  "model": "cheukt:wyze-bridge:conditional-camera",
  "attributes": {
    "camera": "front_door_rtsp",
    "manager": "wyze",
    "camera_name": "Front Door",
    "window_seconds": 20
  }
}
```

When no event is active, a data-management capture returns
`data.ErrNoCaptureToStore` so nothing is stored; otherwise the underlying
camera's frame is returned. When stamping is enabled (opt-in via a `stamp`
config block, off by default), the frame is also tagged with the active Wyze
`event_id` as a classification and a full-frame `wyze_event:<id>` bounding box.
The bounding box makes the event id server-filterable in Viam cloud data (the
data API can't filter on classification labels), which is what lets the
`cheukt:home:home-ui` dashboard group uploaded images by event. Live views are
never stamped.

## Build

```bash
make module.tar.gz     # builds bin/viam-module + downloads go2rtc → the bundle referenced by meta.json
```

It's a pure Go build (plus the pinned go2rtc download) — no Node toolchain
required. The module entry point is `bin/viam-module` (see
[meta.json](../../meta.json)).
