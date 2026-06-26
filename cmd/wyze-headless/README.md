# wyze-headless

A stripped-down variant of [docker-wyze-bridge](../../README.md): Wyze auth +
camera discovery + go2rtc streaming, and nothing else. No WebUI, MQTT, webhooks,
recording, or snapshots.

It's purely additive — a second `cmd/` that reuses the same `internal/` packages
as the full bridge. The full `wyze-bridge` binary is unchanged.

## What it does (and doesn't)

| | wyze-bridge | wyze-headless |
|---|---|---|
| Wyze auth + camera discovery | ✅ | ✅ |
| Embedded go2rtc (RTSP/WebRTC/HLS) | ✅ | ✅ |
| TUTK cameras | ✅ | ✅ |
| WebUI / REST / SSE | ✅ | ❌ |
| MQTT / Home Assistant | ✅ | ❌ |
| Webhooks | ✅ | ❌ |
| Recording / snapshots | ✅ | ❌ |
| KVS doorbells (`GW_BE1`/`GW_DBD`) | ✅ | ❌ |
| Gwell OG (`GW_GC1`/`GW_GC2`) | ✅ | ❌ |
| Auth state persistence | ✅ | ❌ (re-auths each start) |
| External go2rtc (`GO2RTC_URL`) | ✅ | ❌ (embedded only) |

> **TUTK only.** The KVS doorbell path needs the `/internal/wyze/*` loopback
> shim that's served by the (omitted) WebUI, so doorbell-lineage cameras won't
> stream here. Use the full `wyze-bridge` for those.

You consume streams straight from go2rtc — RTSP on `:8554`, WebRTC on `:8889`,
and the go2rtc UI/API (which also serves HLS) on `:1984`.

## Build & run

```bash
make build          # go build -o wyze-headless ./cmd/wyze-headless
make go2rtc         # download the pinned go2rtc binary to ./go2rtc (if missing)
make run            # ./wyze-headless (auto-downloads go2rtc first)
```

`make run` depends on the `go2rtc` target, so it fetches the pinned go2rtc
release for your platform on first use. `findGo2RTCBinary()` looks for `./go2rtc`
first, then `/usr/local/bin`, `/usr/bin`, then `$PATH`.

## Configuration

Config comes from a dotenv file. The path is `ENV_FILE` (default `.env.dev`),
loaded into the process environment before the config struct is built. A real
shell env var always wins over a value in the file.

```bash
cp .env.dev.example .env.dev   # then fill in your credentials
./wyze-headless                # loads ./.env.dev by default
ENV_FILE=./local/prod.env ./wyze-headless
```

A missing file at the **default** path is fine (falls back to the ambient env);
a missing file at an **explicit** `ENV_FILE` is an error.

### Variables

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `WYZE_EMAIL` | ✅ | — | Wyze account email |
| `WYZE_PASSWORD` | ✅ | — | Wyze account password |
| `WYZE_API_ID` | ✅ | — | [Developer API ID](https://developer-api-console.wyze.com/#/apikey/view) |
| `WYZE_API_KEY` | ✅ | — | Developer API Key |
| `WYZE_TOTP_KEY` | — | — | TOTP secret (only if your account has MFA) |
| `BRIDGE_IP` | — | — | Your LAN IP; required for WebRTC ICE candidates |
| `STATE_DIR` | — | `./local/config` | Where `go2rtc.yaml` is written |
| `LOG_LEVEL` | — | `info` | `trace`/`debug`/`info`/`warn`/`error` |
| `FORCE_IOTC_DETAIL` | — | `false` | Verbose go2rtc + bridge logging |
| `ENV_FILE` | — | `.env.dev` | Path to the dotenv file (read from the real env) |

Everything else (quality `hd`, audio on, STUN server, discovery interval, etc.)
is a sensible built-in default. The `loadConfig()` function in
[main.go](main.go) is the single seam to swap for CLI flags or a directly
constructed config later.

## Verify

```bash
go build -o wyze-headless ./cmd/wyze-headless && go vet ./cmd/wyze-headless
./wyze-headless
# Then confirm:
#   go2rtc API/UI   -> http://127.0.0.1:1984
#   RTSP            -> rtsp://127.0.0.1:8554/<camera-name>
#   WebRTC          -> http://127.0.0.1:8889/<camera-name>
```
