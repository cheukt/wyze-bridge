# Wyze Bridge — Viam Module (design)

Design and rationale for the `cheukt:wyze-bridge` Viam module. This is the
developer-facing "why"; user-facing model/config docs live in
[VIAM.md](VIAM.md) and `cmd/viam-module/README.md`. The module is implemented
under `internal/viammod/` + `cmd/viam-module/` — read the code for the "how";
this doc is for the decisions behind it and the work still open.

## Goal

Expose Wyze cameras to a Viam machine. A **generic service** (`manager`) embeds
the wyze-headless core (Wyze auth + camera discovery + embedded go2rtc) and
publishes each camera as a loopback RTSP stream. Standard
**`viam:viamrtsp:rtsp`** cameras consume those streams. The service *produces*
the streams; viamrtsp *consumes* them; the service's `DoCommand` is the
discovery surface that tells you which stream names exist, so wiring the rtsp
cameras is mechanical.

```
Viam machine (viam-server)
├── generic service  cheukt:wyze-bridge:manager    ← this module
│      embeds the wyze core in-process:
│        wyzeapi.NewClient → camera.NewManager → setupGo2RTC → RunDiscoveryLoop
│      Config: creds_file path, ports, filters, log level  (NO secrets)
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

The LAN-local dashboard (camera video + browsable motion-event image history) is
**not** in this repo — it was split out to the separate `cheukt:home` module
(`github.com/cheukt/home`, model `cheukt:home:home-ui`). See
[The cheukt:home split](#the-cheukthome-split) below.

## manager: design decisions

### Why embed (in-process) instead of subprocess

`cmd/wyze-headless` is already the library-shaped core we need (auth + discovery
+ embedded go2rtc, no WebUI/MQTT/recording). Because the module lives **in this
repo** it imports the `internal/` packages directly — `camera.Manager.Cameras()`
returns the live camera list in-process, no HTTP proxy for discovery.

Trade-off accepted: a panic in bridge code lands in viam-server's module process
(no subprocess fault boundary), and the bridge's release cycle is coupled to this
module. In exchange: one binary, no child-process supervision, direct camera
state.

### Keeping go2rtc alive (supervised from viammod, not go2rtcmgr)

`go2rtcmgr.Manager` starts go2rtc once and reaps it; nothing respawns a crash.
The natural fix — a supervisor loop inside `go2rtcmgr` watching the process exit
channel — would edit an upstream-owned file, so
`internal/viammod/go2rtc_supervisor.go` does it from outside, on exported API only:

- **Detection:** poll `IsHealthy` every 5s; restart after 2 consecutive misses.
  Slower than watching the process exit, but it also catches an alive-but-wedged
  go2rtc that an exit watcher misses.
- **Restart:** `Stop` the old process (freeing :1984 before the next port
  pre-flight), then `Start` + `WaitReady` on a **fresh** `Manager` from
  `newGo2RTCManager`. Reusing the old one is a dead end: its `cmd` stays non-nil
  if exec fails, so every later `Start` answers "already running", and its ready
  channel is already closed. Failures back off 2s→60s and retry; a process that
  starts but never answers is stopped so it doesn't hold the ports.
- **Re-registration:** a fresh go2rtc only knows the streams in its YAML — every
  TUTK stream was registered at runtime over the HTTP API. Each restart therefore
  fans out `camMgr.RestartStream` per camera (`ConnectAll` would skip cameras
  still believing they stream). Parallel, since with the health probe on each
  reconnect waits for a real frame.
- **Shutdown:** `Close` cancels `serviceCtx` *before* `Stop`, so a deliberate
  teardown ends the loop instead of reading as a crash.

The full bridge (`cmd/wyze-bridge`, `cmd/wyze-headless`) keeps upstream behavior:
a crashed go2rtc stays down. Fixing that belongs in an upstream PR.

### No Reconfigure (`AlwaysRebuild`)

The service embeds `resource.AlwaysRebuild`. On any config change viam-server
calls `Close` then re-runs the constructor — no diff-the-config logic; the
constructor builds everything, `Close` cancels the context and stops go2rtc.
Cost accepted: every config edit respawns go2rtc and re-auths to Wyze (a few
seconds of stream downtime). For a creds-oriented service that's fine.

### Credentials stay off the Viam cloud

The Viam app stores robot config as plaintext JSON in the cloud, so the four
Wyze secrets never appear in it. The config carries only `creds_file` — a path
to a dotenv-format file **on the machine** (mode `0600`) holding
`WYZE_EMAIL` / `WYZE_PASSWORD` / `WYZE_API_ID` / `WYZE_API_KEY` (+ optional
`WYZE_TOTP_KEY`). `Validate` only enforces a non-blank `creds_file` (it may run
before the file exists / on another host context); the constructor reads and
validates the file, erroring by field name if a required cred is missing. Secrets
are never logged.

Other config fields (all non-secret): `bridge_ip`, `state_dir` (defaults to
`$VIAM_MODULE_DATA`), `rtsp_port` (`8554`), `log_level`, `force_iotc_detail`,
`stun_server`, and `filter_names` / `filter_models` / `filter_macs` /
`filter_block` (an allow-list of streams to expose; the constructor sets the
filter's `AllowEmpty` so an explicit filter matching no cameras exposes nothing).

### DoCommand (discovery surface)

| Command | Action |
|---------|--------|
| `{"list_cameras": true}` | `[{name, nickname, model, state, rtsp_url}]`. Probes each camera by default (verifies media, drops `rtsp_url` + surfaces `error` for cameras that don't produce a frame); `{"list_cameras": {"probe": false}}` for a fast, side-effect-free list |
| `{"get_events": true}` | motion events in the last minute (window overridable) |
| `{"restart_camera": "<name>"}` | `camMgr.RestartStream` |
| `{"set_quality": {"name": "<n>", "quality": "hd"\|"sd"}}` | `camMgr.SetQuality` |

RTSP host defaults to `127.0.0.1`, port to `rtsp_port`. `list_cameras` is the
sole cross-repo contract consumed by `cheukt:home` (see below); `get_events` is
internal (gated camera only).

### Logging (zerolog → Viam, no destructive globals)

Internal packages log through an injected `zerolog.Logger` almost everywhere. A
`zerolog.LevelWriter` adapter maps zerolog levels to the matching
`logging.Logger` method and forwards the line; the zerolog side passes
*everything* (Trace) and the Viam logger does the real filtering at the
operator-set level. To capture the 3 `internal/wyzeapi/state.go` package-global
calls, the constructor reassigns `log.Logger` and calls
`SetGlobalLevel(Trace)` once — benign because viam-server uses zap, not zerolog,
so nothing else reads them.

## conditional-camera

`rdk:component:camera` that wraps an underlying camera (typically a
`viam:viamrtsp:rtsp`) and gates its **data-management captures** on recent Wyze
motion: `Images()` returns `data.ErrNoCaptureToStore` when no event is recent. A
background loop polls the manager's `get_events` (skipping polls within the Wyze
cooldown) and tracks `lastEventTS`.

Its only non-generic dependency is the manager's event feed, reached at runtime
via `resource.FromDependencies` + `manager.DoCommand` — no in-process Go
reference. This is why it stays co-located with the manager: the `get_events`
contract stays internal (guarded by a Go shape-test, no cross-repo drift), and
public users just *configure* it rather than learning a wire shape.

**We keep our own gated camera, not `erh/filtered_camera`** — that module gates
on an on-device vision service; our event source is Wyze cloud motion via
`DoCommand`, and our ~400-line component is leaner. Recorded so it isn't
relitigated.

### Opt-in stamping

When enabled, the component stamps each captured frame with the active event id —
as a classification **and** a full-frame `wyze_event:<id>` bounding box (the bbox
is what makes the id server-filterable in cloud data; the data API can't filter
on classification labels). This is **opt-in via a `stamp` block, default off**
(`{enabled, label_prefix}`), so the public component stays a generic gate;
`cheukt:home` turns it on. See [VIAM.md](VIAM.md) for the config.

## The cheukt:home split

The dashboard was extracted to its own **private** module so the manager +
conditional-camera could go **public** for the Viam ecosystem. Viam visibility is
per-module, so a public manager can't share a module with a private dashboard —
that forces exactly one extraction (the dashboard), and only two repos, not three
(the gated camera's event source *is* the manager, so co-locating them keeps the
`get_events` contract internal).

| repo | module_id | visibility | models |
|---|---|---|---|
| this repo | `cheukt:wyze-bridge` | public (planned) | `manager`, `conditional-camera` (+ optional `discovery`) |
| `github.com/cheukt/home` | `cheukt:home` | private | `home-ui` (dashboard) |

**Cross-repo contract** — two conventions bind `cheukt:home` to this repo, both
consumed only by your own dashboard (no `contract_version` ceremony):

1. **`list_cameras` `DoCommand`** — `{ cameras: [{ name, nickname, model, state,
   rtsp_url?, ready?, error? }] }`. `rtsp_url` always present on the
   `probe=false` fast path; on the probing path only for cameras that produced
   media. Missing `rtsp_url` = "not currently streamable."
2. **`wyze_event:<id>` label schema** — stamped by the gated camera (when
   enabled), read by `cheukt:home` from the Viam Data API
   (`boundingBoxLabelsByFilter` + `binaryDataByFilter`), not over `DoCommand`.
   Prefix is configurable; camera and dashboard must agree on it.

Guard each with a shape-test on each side. This repo's README is the source of
truth for both.

## Fork mergeability (Viam-only identity, clean fork)

This repo is **Viam-only in identity and release** (README leads with Viam, the
published artifact is the Viam module, CI builds the module) while staying a
low-friction fork of the Go upstream `IDisposable/docker-wyze-bridge`.
Governing rule: **merge friction = the number of upstream-owned files you edit or
delete** — minimize both.

- **Don't rename the go.mod module path.** Keep
  `github.com/IDisposable/docker-wyze-bridge` (imported by ~44 files; renaming
  collides with every upstream merge for no gain — a Go module path needn't match
  the repo host). Fix only `meta.json` `url`.
- **Own shared-path files via `.gitattributes merge=ours`** (`README.md`,
  eventually `Makefile`) so `git merge upstream/main` auto-keeps your version.
  Needs `git config merge.ours.driver true` once per clone — capture it in a
  `make setup-fork` target.
- **Delete nothing upstream maintains.** Leave `docker/`, `docker-compose.yml`,
  `setup.sh`, `MIGRATION.md` inert rather than deleting them (deletion creates
  permanent delete/modify conflicts).
- **Keep your code in add-only paths** (`internal/viammod/`, `cmd/viam-module/`,
  `meta.json`) and **out of the core** (`internal/wyzeapi`, `internal/camera`,
  `internal/go2rtcmgr`) — wrap new behavior in `viammod`. The import direction
  viammod → core, never the reverse, is what makes this work.
- **README stays AGPL-compliant** — preserve the Attribution section,
  `THIRD_PARTY_NOTICES.md`, and the license.
- Viam user docs live in [VIAM.md](VIAM.md) (add-only), **not** `MIGRATION.md`
  (upstream-owned).

## Build & ship

- `make module.tar.gz` builds `bin/viam-module` (`CGO_ENABLED=0 -tags no_cgo`)
  and bundles it with `go2rtc` in `bin/` plus `meta.json`. Pure Go build — no
  Node/frontend (that moved to `cheukt:home`), no ffmpeg (RTSP is codec-copy; the
  liveness probe uses go2rtc's `frame.mp4`, not `frame.jpeg`). go2rtc sits next
  to the entrypoint so `findGo2RTCBinary` locates it via `os.Executable()`.
  - **`-tags no_cgo` is required:** `conditional-camera` imports
    `rdk/components/camera`, which transitively pulls gostream's cgo-only audio
    driver; the tag drops it (we need no audio capture).
- `meta.json`: `module_id cheukt:wyze-bridge`, two models (`manager` →
  `rdk:service:generic`, `conditional-camera` → `rdk:component:camera`),
  `entrypoint bin/viam-module`, arches `linux/amd64` / `linux/arm64` /
  `darwin/arm64`. Build artifacts gitignored.
- `make reload` runs `viam module reload --part-id $VIAM_PART_ID` (read from a
  gitignored `.env.viam`, template `.env.viam.example`) for cloud hot-reload.

## Future work

- **Go public** — flip the GitHub repo + `meta.json` visibility to public (only
  after confirming no private source remains), set `meta.json` `url` to
  `github.com/cheukt/wyze-bridge`, Viam-first README, add the `merge=ours`
  attributes + upstream remote. Manager + gated camera unchanged for consumers.
- **notify** (gated camera) — fire on the event *edge* (not the frame path,
  which would spam at capture cadence), reusing the poll loop's `lastEventTS`.
  Track `lastNotifiedEventID`; on a newer id, render a config `template`
  (`text/template`) and `POST` to a `webhook_url` in a goroutine. Requires
  widening `fetchLatestEvent` to also carry `thumbnail_url`. Failures
  log-and-drop; never block gating.
- **discovery** (optional) — a `cheukt:wyze-bridge:discovery`
  `rdk:service:discovery` model in the same module. Depends on `manager` by name;
  `DiscoverResources` calls `list_cameras` with `probe=false` (reuses the live
  session — no second auth) and returns one `viam:viamrtsp:rtsp` config per
  camera using the reported `rtsp_url` **verbatim**. Emits plain rtsp cameras
  only — gating stays a deliberate layer the user adds. Turns "hand-write a
  camera per Wyze cam" into "click Discover."
- **Remote RTSP host override** — advertise `bridge_ip` instead of `127.0.0.1`
  in `list_cameras` for remote consumers.
- **Motion as a standard surface** — expose Wyze motion as a `rdk:service:vision`
  classification or a `sensor` with `{event_active, event_id, event_ts}` readings
  for public users who prefer their own gate. Additive; defer until a second
  consumer asks.
- **Reduce on-machine secret exposure** — warm-start from the persisted
  `AuthState` refresh token (already stored at `0600` in `VIAM_MODULE_DATA`)
  and/or a `DoCommand` bootstrap that takes creds once and lets `creds_file` be
  removed afterward.
- **Gwell/WebRTC-lineage cameras** — headless is TUTK-only today; extend if
  needed.

## Decisions made

- **Discovery surface:** generic-service + `DoCommand list_cameras` (operator
  hand-adds one `viam:viamrtsp:rtsp` per camera). Not implementing the Viam
  Discovery service API for v1.
- **`AlwaysRebuild`:** accepted; no partial `Reconfigure`.
- **Secrets:** on-machine `creds_file` (dotenv, `0600`), never in the Viam cloud.
- **Two repos, not three:** the gated camera stays with the manager.
- **Keep our own gated camera**, not `erh/filtered_camera`.
- **Viam-only = identity/release, not deletion** of upstream files.
- **Don't rename the go.mod module path.**
