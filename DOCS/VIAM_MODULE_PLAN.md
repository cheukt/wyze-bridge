# Viam Module Plan: wyze-bridge as a generic service

> **Historical.** This plan describes the original single-module design, which
> included the `cat-ui` dashboard in this repo. cat-ui has since been split out
> into the separate `cheukt:home` module (`github.com/cheukt/home`, model
> `cheukt:home:home-ui`) — the sections below that describe cat-ui / `catui-dev`
> / `internal/viammod/ui/` no longer reflect this repo. See
> [MODULE_SPLIT_PLAN.md](MODULE_SPLIT_PLAN.md) for the split. The manager +
> `conditional-camera` design here is still accurate.

## Goal

Expose Wyze cameras to a Viam machine. A **generic service** model embeds the
`wyze-headless` core (Wyze auth + camera discovery + embedded go2rtc) and
publishes each camera as an RTSP stream. Standard **`viam:viamrtsp:rtsp`**
camera components consume those streams as their input.

The generic service *produces* the streams; the existing viamrtsp module
*consumes* them. The service's `DoCommand` is the discovery surface that tells
you which stream names exist, so wiring the rtsp cameras is mechanical.

```
Viam machine (viam-server)
├── generic service  cheukt:wyze-bridge:manager    ← this module
│      embeds the wyze-headless core in-process:
│        wyzeapi.NewClient → camera.NewManager → setupGo2RTC → RunDiscoveryLoop
│      Config: wyze creds, ports, log level
│      DoCommand: list_cameras / get_events / restart_camera / set_quality
│      └── go2rtc subprocess ──RTSP 127.0.0.1:8554/<name>──┐
│                                                          │
├── camera  viam:viamrtsp:rtsp  (one per Wyze camera)      │
│      Config: { rtsp_address: "rtsp://127.0.0.1:8554/front_door" } ◄┘
│                                                          ▲
└── camera  cheukt:wyze-bridge:conditional-camera         │ (optional)
       wraps a viamrtsp camera; polls the manager's ──────┘
       get_events and only passes data-management captures
       through when a Wyze motion event is recent.
```

This module registers **three** models: the `manager` generic service (the
core), the `cat-ui` generic service (an optional LAN-local Svelte dashboard —
see "cat-ui web dashboard" below), and an optional `conditional-camera`
component that gates recordings on motion.

## Why embed (in-process) instead of subprocess

`cmd/wyze-headless/main.go` is already the library-shaped core we need: Wyze
auth + camera discovery + embedded go2rtc, with no WebUI/MQTT/recording. Its
`main()` is a short orchestration (`NewClient → NewManager → setupGo2RTC →
RunDiscoveryLoop`) and the author flagged `loadConfig()` as the seam to swap for
"a directly constructed / flag-driven config." A Viam `Config` struct is exactly
that swap target.

Because the module lives **in this repo**, it can import the `internal/`
packages directly (`internal/config`, `internal/camera`, `internal/go2rtcmgr`,
`internal/wyzeapi`). Discovery needs no HTTP proxy: `camera.Manager.Cameras()`
returns the live camera list in-process.

Trade-off accepted: a panic in bridge code lands in viam-server's module
process (no subprocess fault boundary), and the bridge's release cycle is
coupled to this module. In exchange: one binary, no child-process supervision,
no HTTP polling, direct camera state.

## No Reconfigure

The service embeds `resource.AlwaysRebuild`. On any config change viam-server
calls `Close` then re-runs the constructor — no diff-the-config logic, and the
"tear down + rebuild" lifecycle we want comes for free. Constructor builds
everything; `Close` cancels the context and stops go2rtc.

Cost accepted: every config edit respawns go2rtc and re-auths to Wyze (full
discovery loop restart, a few seconds of stream downtime). For a creds-oriented
service that's fine — no partial `Reconfigure` is worth the complexity.

## File layout (new)

| Path | Purpose |
|------|---------|
_Current-repo file map (post cat-ui split). The cat-ui rows from the original
plan — `catui*.go`, `internal/viammod/ui/`, `cmd/catui-dev/` — are gone; that
dashboard now lives in `github.com/cheukt/home`._

| Path | Purpose |
|------|---------|
| `cmd/viam-module/main.go` | Entry: `module.ModularMain` registering the `generic.API`/`manager` service and the `camera.API`/`conditional-camera` component |
| `internal/viammod/wyzebridge.go` | The generic service: `Model`, `init()`+`RegisterService`, `*Config`, `Validate`, constructor, `DoCommand`, `Close` |
| `internal/viammod/wyzebridge_test.go` | `Validate` table tests + `DoCommand` `list_cameras` test via `InjectCamera` |
| `internal/viammod/conditional_camera.go` | The `conditional-camera` component: `ConditionalModel`, `*ConditionalConfig`, event-gated `Images`, background `get_events` poll loop, opt-in `stamp` block |
| `internal/viammod/conditional_camera_test.go` | `Images` gate, poll-skip, `fetchLatestEvent`, and `Validate` tests via fake manager + camera |
| `meta.json` | Module manifest (two models: `manager`, `conditional-camera`) |
| `Makefile` | `make module.tar.gz` (bundles `viam-module` + `go2rtc`; pure Go build, no frontend), `make reload` (cloud hot-reload) |

`go.mod`: `go.viam.com/rdk` (the module is now a pure Go build — the frontend
`@viamrobotics/sdk` dependency moved to `github.com/cheukt/home`).

## Component details

### 1. Model & registration
```go
var Model = resource.NewModel("cheukt", "wyze-bridge", "manager")

func init() {
    resource.RegisterService(generic.API, Model,
        resource.Registration[resource.Resource, *Config]{Constructor: newService})
}
```

### 2. Config (replaces wyze-headless `loadConfig` dotenv path)

**Credentials never appear in the Viam config.** The Viam app stores robot
config as plaintext JSON in the cloud, so the four Wyze secrets live in a file
**on the machine** and the config carries only its path (see "Credential
handling" below). The Viam `Config` JSON is non-secret:

| JSON field | Maps to | Notes |
|------------|---------|-------|
| `creds_file` | (path to on-machine creds file) | **required, non-blank** — dotenv-format file holding the Wyze creds |
| `bridge_ip` | `BridgeIP` | optional, WebRTC ICE host |
| `state_dir` | `StateDir` | default `$VIAM_MODULE_DATA`, falling back to `./local/config` when that env is unset |
| `rtsp_port` | (go2rtc RTSP listen port) | default `8554`; threaded into the go2rtc config and the `rtsp_url`s reported by `list_cameras` |
| `log_level` | `LogLevel` | default info |
| `force_iotc_detail` | `ForceIOTCDetail` | optional verbose |
| `stun_server` | `STUNServer` | default `stun:stun.l.google.com:19302` |
| `filter_names` | `FilterNames` | optional allow-list by camera nickname (case-insensitive); empty = expose all |
| `filter_models` | `FilterModels` | optional, by model code or human-readable model name |
| `filter_macs` | `FilterMACs` | optional, by MAC address |
| `filter_block` | `FilterBlocks` | optional; when true, matched cameras are *excluded* instead of included |

The `filter_*` fields are normalized (uppercased/trimmed) before reaching the
camera `Filter`, matching what the env path's `envList` does. Because a filter
here is an intentional allow-list of streams to expose, the constructor sets the
filter's `AllowEmpty` flag: an explicit filter that matches **no** cameras
exposes nothing, rather than the env path's "never filter to empty" fallback to
all cameras.

Literals carried over from headless `loadConfig`: `RefreshInterval: 30m`,
`Quality: "hd"`, `Audio: true`, `BridgePort: 5080`, `GwellEnabled: false`,
`CamOverrides: map{}`.

`state_dir` resolution: viam-server injects `VIAM_MODULE_DATA` as the
persistent per-module data directory, so the constructor defaults `StateDir` to
that (where `go2rtc.yaml` and Wyze state live across rebuilds), falling back to
headless's `./local/config` only when the env is absent (e.g. local dev runs).

#### Credential handling (secrets stay off the Viam cloud)

The `creds_file` is a dotenv-format file on the machine (mode `0600`, outside
the Viam config), holding the same keys headless reads from `.env`:

```
WYZE_EMAIL=...
WYZE_PASSWORD=...
WYZE_API_ID=...
WYZE_API_KEY=...
WYZE_TOTP_KEY=...   # optional (MFA)
```

Reuse headless's `loadEnvFile` to parse it (the file path, not env, is the
input). The constructor reads the file, builds `wyzeapi.Credentials`, and errors
if any of the four required creds are absent **from the file**. Secrets are
never logged (redact in the §6 adapter; `Validate`/constructor errors reference
field names only). Operators provision the file out-of-band (the operator docs
below cover creating it at `0600`).

`Validate(path)`:
- rejects a **blank `creds_file`** (`"creds_file" is required`) — this is the
  only hard requirement;
- does **not** parse the file contents (Validate may run before the file is in
  place / on a different host context); the actual cred-presence check happens
  in the constructor, which errors clearly if the file is missing or incomplete;
- returns no resource dependencies — the service is self-contained.

### 3. Constructor (`newService`)

Ports the body of `wyze-headless` `main()`:
1. Load `creds_file` (reuse `loadEnvFile`), build `wyzeapi.Credentials`, and
   error if any required cred is missing/blank (mirror `creds.IsSet()`). Then
   build `*config.Config` from the Viam `Config` (resolve `StateDir` and
   `RTSPPort`/`8554` here). Creds come from the file, never the Viam config.
2. Create a cancelable `serviceCtx` (stored for `Close`).
3. Build the zerolog→Viam adapter logger (see §6) and use sub-loggers of it for
   every component constructed below.
4. `apiClient := wyzeapi.NewClient(creds, version, log)`
5. `camMgr := camera.NewManager(cfg, apiClient, nil, log)`
6. `go2rtcAPI, go2rtcMgr := setupGo2RTC(serviceCtx, cfg, log)` (port the
   helper from headless; **extend `findGo2RTCBinary` to also probe the module
   entrypoint's own directory via `os.Executable()`** — the bundled `go2rtc`
   ships next to `bin/viam-module`, which headless's probe list doesn't cover.
   Pass `rtsp_port` into the go2rtc config builder).
7. `camMgr.SetGo2RTCAPI(go2rtcAPI)`
8. `go camMgr.RunDiscoveryLoop(serviceCtx)`
9. Store `camMgr`, `go2rtcMgr`, `cancel` on the struct.

### 4. DoCommand (discovery surface)

| Command | Action |
|---------|--------|
| `{"list_cameras": true}` | iterate `camMgr.Cameras()` → `[{name, nickname, model, state, rtsp_url: "rtsp://127.0.0.1:<rtsp_port>/<name>"}]`. Probes each camera by default (verifies media, drops `rtsp_url` + surfaces `error` for cameras that don't produce a frame); pass `{"list_cameras": {"probe": false}}` for a fast, side-effect-free list |
| `{"get_events": true}` | motion events in the last minute (window overridable) |
| `{"restart_camera": "<name>"}` | `camMgr.RestartStream(ctx, name)` |
| `{"set_quality": {"name": "<n>", "quality": "hd"\|"sd"}}` | `camMgr.SetQuality(ctx, name, quality)` |

The RTSP host defaults to `127.0.0.1` (rtsp camera runs on the same machine) and
the port to the configured `rtsp_port` (`8554`); expose an optional host
override later if remote consumers are needed.

### 5. Close

`cancel()` the service context, then `go2rtcMgr.Stop()` (cascades to the go2rtc
subprocess). Idempotent.

### 6. Logging (zerolog → Viam, no destructive globals)

The internal packages log through an **injected** `zerolog.Logger`
(`m.log`, `apiLog`, …) almost everywhere; the only exceptions are 3 calls in
`internal/wyzeapi/state.go` that use the zerolog package global. So a writer
adapter captures essentially everything:

- Implement a `zerolog.LevelWriter` whose `WriteLevel(lvl, p)` maps the zerolog
  level to the matching `logging.Logger` method (Error/Warn → Error/Warn,
  Debug/Trace → Debug, else Info) and forwards the line.
- Construct `zl := zerolog.New(adapter).Level(zerolog.TraceLevel).With().Timestamp().Logger()`
  and inject component sub-loggers of it everywhere headless passes a logger.
- **Layering:** the zerolog side passes *everything* through (Trace); the Viam
  `logging.Logger` does the real filtering at the level the operator set per
  resource in the Viam app. The `log_level` config field just sets that Viam
  logger's level — it is **not** a zerolog global.
- To also capture the 3 `state.go` global calls, reassign `log.Logger = zl`
  once in the constructor, and call `zerolog.SetGlobalLevel(zerolog.TraceLevel)`
  once. These are the only deliberate global writes; both are benign because
  viam-server uses zap (not rs/zerolog), so nothing else in the process reads
  them, and widening the global gate is non-destructive (filtering happens
  downstream in Viam). Idempotent across `AlwaysRebuild` cycles.

## Camera wiring (operator docs, not code)

1. On the machine, create the creds file (kept off the Viam cloud):
   ```bash
   sudo install -m 600 /dev/null /etc/wyze-bridge/wyze.env
   sudo tee /etc/wyze-bridge/wyze.env >/dev/null <<'EOF'
   WYZE_EMAIL=...
   WYZE_PASSWORD=...
   WYZE_API_ID=...
   WYZE_API_KEY=...
   EOF
   ```
2. Add the `cheukt:wyze-bridge:manager` generic service with
   `{ "creds_file": "/etc/wyze-bridge/wyze.env" }` (no secrets in this config).
3. Call its `list_cameras` DoCommand to get stream names + URLs.
4. For each camera, add a `viam:viamrtsp:rtsp` component:
   ```json
   { "rtsp_address": "rtsp://127.0.0.1:8554/front_door" }
   ```
5. (Optional) To record only around motion, wrap the rtsp camera in a
   `cheukt:wyze-bridge:conditional-camera` and capture *that* instead. It polls
   the manager's `get_events` and drops data-management captures when no Wyze
   motion event is recent. See `cmd/viam-module/README.md` for its attributes.
   Each captured frame is stamped with the active Wyze `event_id` — as both a
   classification and a full-frame bounding box labeled `wyze_event:<id>` (the
   bbox is what makes the id server-filterable in cloud data; the data API can't
   filter on classification labels). The cat-ui events panel groups uploaded
   images by that id (see below).

## cat-ui web dashboard

`cheukt:wyze-bridge:cat-ui` is an **optional** second generic service that
serves a small Svelte dashboard (camera video + a browsable history of uploaded
motion-event images) over HTTP **directly on the machine**. The Go side is a
thin static-file server; all camera, manager, and cloud-data traffic flows
**browser→Viam over the Viam TS SDK**, not through this service.

```
browser ──http──► cat-ui service :ui_port
  │                 ├─ GET /              embedded Svelte SPA (//go:embed ui/dist)
  │                 └─ GET /config.json   {host, apiKey(Id), signaling, manager, cameras, partId}
  │                          (from VIAM_MACHINE_FQDN / VIAM_API_KEY(_ID) / VIAM_MACHINE_PART_ID
  │                           injected by viam-server)
  │
  ├── Viam SDK (WebRTC) ──► machine ── StreamClient.getStream(cam)  → viamrtsp camera video
  │                                 └─ GenericServiceClient(manager).doCommand → status/cameras
  └── Viam SDK (app) ────► app.viam.com ── DataClient: boundingBoxLabelsByFilter + binaryDataByFilter
                                            (uploaded event images, scoped by partId)
```

- **Why this shape.** The `viam:viamrtsp:rtsp` cameras that consume the manager's
  RTSP streams are ordinary Viam camera components, which viam-server already
  streams over its own authenticated WebRTC transport. Reusing that for video
  means no go2rtc proxy, no exposed media port, NAT traversal for free, and it
  works remotely — not just on the LAN. The manager is untouched and cat-ui has
  **no Viam resource dependency** (the browser calls the manager, not the Go
  service).
- **Credentials.** viam-server injects `VIAM_MACHINE_FQDN`, `VIAM_API_KEY`,
  `VIAM_API_KEY_ID`, and `VIAM_MACHINE_PART_ID` into every module process
  (`go.viam.com/rdk/utils` `MachineFQDNEnvVar` / `APIKeyEnvVar` /
  `APIKeyIDEnvVar` / `MachinePartIDEnvVar`). The constructor reads them and
  serves them at `/config.json` (marked `no-store`), so the browser SDK
  authenticates with **no operator setup**. The part id scopes cloud-data
  queries to this machine; if an older viam-server doesn't inject it, the events
  panel degrades to a "cloud data unavailable" notice (video still works).
  Trade-off: `/config.json` exposes a machine API key to anyone who can reach
  `ui_port` — bind `ui_bind` to a trusted interface / firewall the port.
  (Accepted per the "keep the auth model simple" decision.)
- **Config** (all non-secret): `manager` (required — for the browser's
  `DoCommand`), `cameras` (optional explicit Viam camera names; empty =
  auto-discover all camera components via `robot.resourceNames()`), `ui_port`
  (default `5000`), `ui_bind` (default all interfaces), `signaling_address`
  (default `https://app.viam.com:443`). On startup the constructor logs at
  **info** the browsable `http://<ip>:<port>` URL(s) — a specific `ui_bind`
  verbatim, otherwise the machine's non-loopback IPv4s (`dashboardURLs`).
- **Frontend.** `internal/viammod/ui/` is a Vite + Svelte SPA on
  `@viamrobotics/sdk`: `viam.js` loads `/config.json`, `createRobotClient(...)`,
  then `StreamClient.getStream(name)` per camera (`<video srcObject>`) and
  `GenericServiceClient(robot, manager).doCommand(...)` for `list_cameras`. The
  events panel uses `createViamClient(...).dataClient`: `listEvents()` calls
  `boundingBoxLabelsByFilter({partId})` to find the `wyze_event:*` labels, then a
  paginated `binaryDataByFilter({partId, bboxLabels})` sweep grouped by event id.
  Images render straight from each item's signed `metadata.uri` (no binary
  download); clicking an event drills into its full image set. Polled gently
  (30s) since captures change slowly.
- **Build.** `make ui` (`npm ci && vite build → ui/dist`), embedded via
  `//go:embed all:ui/dist`. A committed `ui/dist/.gitkeep` keeps the embed valid
  on a fresh checkout; `go build` without Node embeds the placeholder and the
  SPA route returns a "not built" notice. `make module.tar.gz` depends on
  `make ui`. `node_modules` + `dist/*` (except `.gitkeep`) gitignored;
  `package-lock.json` committed for `npm ci`. (Bundle is ~275 kB / 64 kB gz —
  the SDK's weight.)

### Local dev (`make run-ui`)

`cmd/catui-dev` iterates on the UI without redeploying the module — and is **not**
a robot client. It serves the SPA + `/config.json` on `:5057` from `.env.viam`
(`VIAM_MACHINE_FQDN` / `VIAM_API_KEY` / `VIAM_API_KEY_ID`, plus
`VIAM_MACHINE_PART_ID` — falling back to `VIAM_PART_ID` so the events panel works
without setting the part id twice) via the exported
`viammod.ServeUI(ctx, UIConfig, addr, logger)`. `make run-ui` runs that backend
plus vite HMR on `:5173` (proxying only `/config.json`). The browser connects to
Viam over the SDK, so video + events are live with no tunnel.

## Tests

- `Config.Validate`: table tests — blank/missing `creds_file` → error; a
  non-blank `creds_file` → ok, no deps. (Validate does not read the file.)
- Creds loading: point the loader at a temp dotenv file; assert all four creds
  are parsed and that a file missing a required cred errors with a field-named
  message and no secret value in the error text.
- `DoCommand` `list_cameras`: construct a `camera.Manager`, seed it with
  `InjectCamera(...)`, assert the returned list + RTSP URLs. No real Wyze auth.

## Build & ship

- `Makefile` target `module.tar.gz` builds `bin/viam-module`
  (`CGO_ENABLED=0 -tags no_cgo`, `-ldflags` stamps `internal/viammod.Version`)
  and bundles it with `go2rtc`
  (downloaded by the existing `go2rtc` target) **into `bin/` together** plus
  `meta.json`. go2rtc sits next to the entrypoint so
  `findGo2RTCBinary` locates it via `os.Executable()`. No ffmpeg: RTSP is a
  codec-copy passthrough, and the liveness probe uses go2rtc's `frame.mp4`
  (H264/H265 copy) rather than `frame.jpeg` (which would shell out to ffmpeg
  to encode). Built natively (Viam cloud build runs on the target arch).
  - **`-tags no_cgo` is required:** the `conditional-camera` component imports
    `rdk/components/camera`, which transitively pulls `rdk/gostream`'s cgo-only
    mediadevices audio driver (`gen2brain/malgo`). Under `CGO_ENABLED=0` that
    driver fails to link (`undefined: malgo.AllocatedContext`); gostream guards
    it behind `//go:build !no_cgo`, so the tag drops it. We need no audio
    capture, so this is free. (Before the camera model, the module imported
    only the generic service and built cleanly without the tag.)
- `meta.json`: `module_id: cheukt:wyze-bridge`, two models
  `cheukt:wyze-bridge:manager` (api `rdk:service:generic`) and
  `cheukt:wyze-bridge:conditional-camera` (api `rdk:component:camera`),
  `entrypoint: bin/viam-module`, build via
  `make module.tar.gz`, arches `linux/amd64`, `linux/arm64`, `darwin/arm64`.
- Build artifacts (`bin/`, `module.tar.gz`) are gitignored.
- `Makefile` target `reload` runs `viam module reload --part-id $VIAM_PART_ID`
  for cloud hot-reload to a running machine part. `VIAM_PART_ID` is read from a
  gitignored `.env.viam` (template: `.env.viam.example`). See DEVELOPER.md.

## Implementation status (done)

All slices below are implemented, building, and tested (`go test ./...`,
`go vet`, gofmt clean; `make module.tar.gz` produces a ~25 MB bundle):

- `internal/viammod/config.go` — `Config` + `validate`.
- `internal/viammod/creds.go` — `loadCredsFile` (dotenv → `wyzeapi.Credentials`,
  no process-env mutation; missing keys named, no values leaked).
- `internal/viammod/logadapter.go` — zerolog→Viam `LevelWriter` +
  `captureZerologGlobals` (reassigns `log.Logger`, `SetGlobalLevel(Trace)`).
- `internal/viammod/go2rtc.go` — module-safe `setupGo2RTC` (errors not Fatal) +
  `findGo2RTCBinary` (probes `os.Executable()` dir).
- `internal/viammod/wyzebridge.go` — `Model`, `init`/`RegisterService`, rdk
  `Validate` (3-return `(required, optional, err)` per rdk v0.132.0),
  `newService`, `DoCommand`, `Close`.
- `internal/viammod/conditional_camera.go` — `conditional-camera` component:
  `ConditionalModel`, `*ConditionalConfig`, event-gated `Images` (returns
  `data.ErrNoCaptureToStore` when no recent event), background `get_events`
  poll loop that skips polling within the Wyze cooldown.
- `cmd/viam-module/main.go` — `module.ModularMain` registering both models.
- `*_test.go` — `validate`/creds tests + `DoCommand list_cameras` via
  `InjectCamera` and error-path tests; conditional-camera gate/poll/fetch tests.
- `meta.json` (two models), `Makefile` `module.tar.gz`.

## Decisions made

- **Discovery surface:** stay with the generic-service + `DoCommand`
  `list_cameras` approach (operator reads stream names, hand-adds one
  `viam:viamrtsp:rtsp` per camera). Not implementing Viam's Discovery service
  API (auto-populated component configs) for v1.
- **`AlwaysRebuild`:** accepted; no partial `Reconfigure` (see "No Reconfigure").
- **cat-ui video/data via the Viam SDK (not go2rtc):** the dashboard's video +
  status/events run browser→machine over the Viam TS SDK (`StreamClient` +
  manager `DoCommand`), and cat-ui is reduced to a static-file + `/config.json`
  server. Rejected the earlier go2rtc `/ws` proxy (needed a reachable go2rtc
  port + SSH tunnel in dev, LAN-only). The Go server still serves the SPA on the
  machine to keep the auth model simple; credentials come from the
  `VIAM_API_KEY(_ID)` / `VIAM_MACHINE_FQDN` env vars viam-server injects, handed
  to the browser via `/config.json`. Accepted trade: that endpoint exposes a
  machine API key to any client that can reach `ui_port`.
- **Secrets:** Wyze creds live in an on-machine `creds_file` (dotenv, `0600`),
  never in the Viam cloud config. `Validate` only enforces a non-blank
  `creds_file`; the constructor reads/validates the file. Logs redact creds.
  (Refresh-token reuse / DoCommand bootstrap deferred — see Open questions.)

## Open questions / later

- Implement the Viam Discovery API later to auto-populate the rtsp camera
  configs, if the manual two-step wiring proves annoying at scale.
- Remote RTSP host override (advertise `bridge_ip` instead of `127.0.0.1`).
- Whether to also surface go2rtc HLS/WebRTC URLs in `list_cameras`.
- Gwell/WebRTC-lineage cameras: headless is TUTK-only today; extend later if
  needed.
- Reduce on-machine secret exposure further: warm-start from the persisted
  `AuthState` refresh token (`StateFile` already stores it at `0600` in
  `VIAM_MODULE_DATA`) and/or a `DoCommand` bootstrap that takes creds once,
  persists the token, and lets the `creds_file` be removed afterward.
