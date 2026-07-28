# Module Split & Public Release Plan

## Goal

Publish the Wyze bridge **manager publicly** so anyone in the Viam ecosystem can
use Wyze cameras (auth + discovery + loopback RTSP) on a machine, while keeping
the bespoke **cat-ui** dashboard **private**. Viam visibility is per-module, so
that single goal forces exactly **one** extraction: cat-ui leaves to its own
private module. Everything else — manager, event-gated camera, optional
discovery — stays here, and this repo flips to public.

Two repos. (An earlier draft proposed three, with the gated camera in its own
public repo; rejected — the gated camera's event source *is* the manager, so
co-locating them keeps a compile-time guard and avoids publishing a bespoke
`DoCommand` contract to strangers.)

### End state

| repo | module_id | visibility | models | why here |
|---|---|---|---|---|
| **this repo** (`cheukt/wyze-bridge`) | `cheukt:wyze-bridge` | **public** (flip from private) | `manager` (generic svc), `conditional-camera` (gated camera), `discovery` (discovery svc, optional) | manager owns the wyze core; gated camera's event source is the manager; discovery reuses the manager session |
| new: `cat-ui` | `cheukt:cat-ui` | **private** | `cat-ui` (generic svc, dashboard) | bespoke "cat" branding + `wyze_event` grouping; must not ride in a public tree |

## Why two repos, not three

1. **Real public audience.** A Wyze→Viam bridge (auth, discovery, loopback RTSP
   consumed by `viam:viamrtsp:rtsp` cameras) is genuinely reusable by anyone with
   a Wyze cam on a Viam machine — that's what justifies going public at all.
2. **Per-module visibility forces out only cat-ui.** One `visibility` per module,
   so a public manager can't share a module with a private dashboard. cat-ui (cat
   branding, `wyze_event` convention, private machine) is the *only* piece that
   must be private, hence the *only* forced extraction.
3. **The gated camera belongs with the manager.** Its only non-generic dependency
   is the manager's event feed, reached at runtime via `DoCommand`. Same repo →
   the `get_events` contract stays internal (guarded by a Go test, no cross-repo
   drift), and public users just *configure* the component instead of learning a
   wire shape. Its rdk-only imports never justified a third repo: its input is
   tied to *this* manager, its output to *cat-ui's* data model.

**Keeping our own gated camera, not `filtered_camera`.** That module
([erh/filtered_camera](https://github.com/erh/filtered_camera)) is the generic
version — it gates data-capture on a vision service within a
`window_seconds_before/after` — but our event source is Wyze cloud motion via
`DoCommand`, not an on-device vision service, and our ~400-line component is
leaner than a dependency we find bloated. Recorded so it isn't relitigated.

## Current coupling (why this is cheap)

All cross-model communication already goes over Viam's cross-module plane, not
in-process Go references:

- `conditional_camera.go` imports **only** rdk; reaches the manager via
  `resource.FromDependencies` + `manager.DoCommand`
  ([:145](../internal/viammod/conditional_camera.go#L145), [:228](../internal/viammod/conditional_camera.go#L228)).
  It stays in this repo, so its `asInt` use from `events.go` is a non-issue —
  nothing is copied out.
- `catui.go` has **no** Viam resource dependency; the browser does the DoCommand
  ([catui.go:69-72](../internal/viammod/catui.go#L69-L72)). The `catui*.go` files
  reference no core helpers (only their own `UIConfig`), so cat-ui moves cleanly
  — grep to confirm before the cut.
- Only `wyzebridge.go` / `events.go` / `creds.go` / `go2rtc.go` import the wyze
  core (`internal/wyzeapi`, `internal/camera`, `internal/go2rtcmgr`) — none of
  which cat-ui touches.

There is **no shared Go library to extract**; the only shared things are the
wire conventions below.

## Cross-repo contract (the one real tax)

With cat-ui private, **two** conventions bind it to this public repo — both
consumed only by **your own** cat-ui, so cheap, but no longer co-editable in one
compiler-checked commit. Document both in this repo's README (the source of
truth) and guard each with a shape-test on each side. **No `contract_version`
field** — the sole external consumer is your own cat-ui and the contract hasn't
changed since the models were written. Field names are exact; `?` marks fields
omitted when empty.

1. **`list_cameras` `DoCommand`** — produced at
   [wyzebridge.go:263](../internal/viammod/wyzebridge.go#L263), consumed by the
   cat-ui browser via the TS SDK (`{ list_cameras: { probe: false } }`,
   [ui/src/lib/viam.js:40](../internal/viammod/ui/src/lib/viam.js#L40)):
   `{ cameras: [{ name, nickname, model, state, rtsp_url?, ready?, error? }] }`
   — `rtsp_url` always present on the `probe=false` fast path (all cat-ui uses);
   on the default probing path only for cameras that produced media, alongside
   `ready` (bool) and, on failure, `error`. Missing `rtsp_url` = "not currently
   streamable."
2. **`wyze_event:<id>` label schema** — the gated camera stamps it (classification
   + full-frame bbox) when stamping is enabled; cat-ui reads it from the **Viam
   Data API** (`boundingBoxLabelsByFilter` + `binaryDataByFilter`,
   [ui/src/lib/viam.js:164](../internal/viammod/ui/src/lib/viam.js#L164)), **not**
   over `DoCommand`. Prefix is configurable (`stamp.label_prefix`); camera and
   cat-ui must agree on it.

**Internal, not cross-repo:** `get_events` (produced by `shapeEvent`,
[events.go:64](../internal/viammod/events.go#L64)) is consumed only by the gated
camera's `fetchLatestEvent` and notify — both in this repo. Guard it with a Go
shape-test; it's not a published contract. Shape for reference:
`{ events: [{ mac, camera?, nickname?, model?, event_ts?, time?, value?, event_id?, tags?, thumbnail_url?, video_url? }], window_seconds }`
— `event_ts` epoch millis (int); `time` same instant RFC3339; image is
`thumbnail_url` (signed), clip `video_url`; `camera`/`nickname`/`model` only when
the MAC resolves to a known cam.

## Opt-in stamping (makes the gated camera public-clean)

Today the gated camera **always** stamps `wyze_event:<id>` onto captured frames
([conditional_camera.go:334](../internal/viammod/conditional_camera.go#L334)) —
hardcoding a cat-ui convention into a soon-to-be-public generic component. Make
stamping **opt-in and prefix-configurable, default off**: the public component
stays a generic gate; cat-ui's private config turns it on.

Sketch (all in [conditional_camera.go](../internal/viammod/conditional_camera.go)):

```go
// ConditionalConfig gains:
	// Stamp controls whether captured data-management frames are stamped with
	// the active event id (classification + full-frame bbox) so uploads are
	// groupable by event. Optional; absent → no stamping (generic gate).
	Stamp *StampConfig `json:"stamp,omitempty"`

type StampConfig struct {
	Enabled     bool   `json:"enabled,omitempty"`      // false/absent → pass through unstamped
	LabelPrefix string `json:"label_prefix,omitempty"` // default "wyze_event:" when enabled
}
```

- Rename const `eventClassPrefix` → `defaultEventClassPrefix` (still
  `"wyze_event:"`), used only as the fallback when enabled without a prefix.
- Resolve `stampEnabled`/`stampPrefix` once in `newConditionalCamera`; store on
  the running struct.
- In `Images()`, gate the call: `if cc.stampEnabled && id != "" { images =
  stampEventID(images, cc.stampPrefix+id) }`. Change `stampEventID` to take the
  finished label rather than computing the prefix itself.
- Config-test cases: absent block, `enabled:true` (default prefix), and
  `enabled:true` + custom prefix — asserting labels on returned annotations.

**Behavior change:** always-on → default-off, so the existing cat-ui setup must
add `"stamp": {"enabled": true}`. (One-line flip if you'd rather default on by
treating `Stamp == nil` as enabled — but default-off is the point.)

## Target layout

### This repo (`cheukt/wyze-bridge`) — public

Keeps manager + gated camera (+ optional discovery); loses only cat-ui and its
UI build.

```
cmd/
  viam-module/    main.go → registers manager, conditional-camera (+ discovery); no cat-ui
  wyze-headless/  (unchanged CLI)
  gwell-proxy/    (unchanged)
internal/viammod/   wyzebridge.go events.go creds.go go2rtc.go config.go logadapter.go
                    conditional_camera.go (stays; stamping now opt-in)
                    (catui*.go, ui/, cmd/catui-dev REMOVED — moved to cat-ui repo)
meta.json           module_id cheukt:wyze-bridge, visibility → public, drop cat-ui model
```

- `make module.tar.gz` / `setup.sh` **lose the npm/Node step** → faster cloud
  builds. go2rtc bundling stays.
- **Keep the go.mod module path as-is** (`github.com/IDisposable/docker-wyze-bridge`)
  — it's imported by 44 files, and renaming it to match the repo would collide
  with every upstream merge for zero benefit (a Go module path needn't match the
  repo host). Fix only `meta.json` `url` to point at `github.com/cheukt/wyze-bridge`.
  See *Viam-only, but a clean fork* below.

### New repo `cat-ui` — private

Standalone Go module, rdk-only deps + the Svelte UI.

```
cmd/module/main.go     → registers cat-ui model (rdk:service:generic)
cmd/catui-dev/         (moved — dev server, make run-ui)
catui/                 catui.go catui_http.go catui_dev.go catui_test.go
                       + ui/ (Svelte source, //go:embed dist)
meta.json              module_id cheukt:cat-ui, visibility private
Makefile               ui + build → module.tar.gz; keeps setup.sh (Node for vite)
go.mod                 module github.com/cheukt/cat-ui
README.md              the DoCommand shapes + wyze_event: label schema it consumes
```

`setup.sh` (Node) lives only here; this repo's build needs no Node. Two CI
pipelines instead of one — the real recurring cost of the split.

## Viam-only, but a clean fork (upstream mergeability)

This repo becomes **Viam-only in identity and release** — the README leads with
Viam, the published artifact is the Viam module, CI builds the module — while
staying a low-friction fork of the Go upstream `IDisposable/docker-wyze-bridge`
(which the go.mod path already names). The mrlt8 repo is *Python*: a source of
protocol knowledge to port by hand, not a mergeable remote.

**Governing rule: merge friction = the number of upstream-owned files you edit or
delete.** Minimize both. "Viam-only" is about identity and release, *not*
deleting the engine you keep pulling.

- **Don't rename the go.mod module path.** 44 files import it; renaming collides
  with every upstream merge for no gain (a Go module path needn't match the repo
  host). Keep `github.com/IDisposable/docker-wyze-bridge`; fix only `meta.json`
  `url`.
- **Own a few shared-path files via `.gitattributes merge=ours`**, so
  `git merge upstream/main` auto-keeps your version and never conflicts on them:
  ```
  README.md   merge=ours
  Makefile    merge=ours   # once you fully Viam-ify it
  ```
  plus `git config merge.ours.driver true`. This is what lets the README be a
  Viam-first landing page while upstream keeps editing theirs.
  > **Note:** `merge.ours.driver` is *local git config*, not committed, so it
  > must be set once per clone. Capture it in the README's contributor section or
  > a `make setup-fork` target so anyone doing upstream merges runs it too.
- **Delete nothing upstream maintains.** Removing `docker/`, `docs/`,
  `docker-compose.yml`, `setup.sh` creates permanent delete/modify conflicts.
  Stop *publishing/advertising* the Docker image, but leave its files inert —
  they cost nothing and save every future merge.
- **Keep your code in add-only paths** — `internal/viammod/`, `cmd/viam-module/`,
  `meta.json`, a new `.github/workflows/viam.yml`. Upstream has none of these, so
  they never conflict. (This already holds; the import direction viammod → core,
  never the reverse, is what makes it work.)
- **Keep edits out of the core.** Modifying `internal/wyzeapi`,
  `internal/camera`, `internal/go2rtcmgr`, etc. risks conflicts on exactly the
  files you want to keep pulling — wrap new behavior in `viammod` instead. This
  discipline matters more than any single rule above.
- **README stays AGPL-compliant.** A Viam-first rewrite must preserve the
  Attribution section, `THIRD_PARTY_NOTICES.md`, and the license (AGPL v3, plus
  the vendored MIT go2rtc/gwell notices).

**Workflow:**
```
git remote add upstream https://github.com/IDisposable/docker-wyze-bridge
git fetch upstream && git merge upstream/main
# merge=ours files resolve automatically; real conflicts only in core you edited.
```

## Phases

Ordered — 0, 4, 5 are independent; 1→2→3 are sequential. Two rules set the order:
**extract before you delete** (cat-ui must be published from its private repo
before it's removed here) and **make nothing public until the private source is
gone** (don't flip the GitHub repo or `meta.json` public while `catui*.go` +
`ui/` are still in the tree — that would leak the private dashboard into a public
tree, the exact thing the split exists to prevent).

- **Phase 0 — opt-in stamping** (independent; can go first). Make stamping opt-in
  in this repo (sketch above), add `"stamp": {"enabled": true}` to the existing
  cat-ui machine config so nothing regresses, publish.
- **Phase 1 — new private repo `cat-ui`.** Move `catui*.go` + `ui/` +
  `cmd/catui-dev`, new scaffolding, `go.mod github.com/cheukt/cat-ui`. `go test`,
  verify `make run-ui` (needs `.env.viam`), publish `cheukt:cat-ui` **private**.
  The machine still runs the old `cheukt:wyze-bridge:cat-ui` — nothing broken yet.
- **Phase 2 — slim this repo + cut over the model** (still private). (1) Edit the
  machine config's cat-ui triplet to `cheukt:cat-ui:cat-ui` and confirm it runs —
  config-edit **before** re-publish, or the machine breaks the moment the old
  model disappears; (2) *then* remove `catui*.go` + `ui/` + `cmd/catui-dev`, strip
  the cat-ui model from `cmd/viam-module/main.go` and the npm/ui bits from the
  Makefile + `setup.sh`, and re-publish. The private source is now gone from this
  tree.
- **Phase 3 — go public.** Only now that cat-ui source is removed: make the GitHub
  repo public, set `meta.json` visibility public and its `url` to
  `github.com/cheukt/wyze-bridge` (leave the go.mod module path untouched — see
  fork maintenance below), document the contract in the Viam-first README,
  re-publish. Manager + gated camera unchanged for consumers.
- **Phase 4 — notify** (independent). Add `notify.go` to the gated camera (design
  below).
- **Phase 5 (optional) — discovery.** Add the `discovery` model to
  `cmd/viam-module` (design below).

## Breaking changes / machine reconfig

Only **one** model triplet changes:

| before | after |
|---|---|
| `cheukt:wyze-bridge:manager` | *(unchanged)* |
| `cheukt:wyze-bridge:conditional-camera` | *(unchanged)* |
| `cheukt:wyze-bridge:cat-ui` | `cheukt:cat-ui:cat-ui` |

Plus the stamping default flips to off — add `"stamp": {"enabled": true}` to the
gated camera's config to preserve today's behavior. Only affects the user's own
machines; document both in the Viam module doc (`DOCS/VIAM.md`), not the Docker
`MIGRATION.md` (that's an upstream-owned Python→Go doc — keep Viam changes out of
it for mergeability).

## Notify feature (Phase 4 design)

Lives in the gated camera. Generic because all app-specific content is **config**,
not code — it's orthogonal to the split and could ship independently. Fires on the
**event edge**, not the frame path, reusing the poll loop's `lastEventTS` tracking
([conditional_camera.go:169](../internal/viammod/conditional_camera.go#L169)).

**Config** (all optional; absent → no notify):
```json
"notify": {
  "webhook_url": "https://discord.com/api/webhooks/…",
  "template": { "content": "🐱 {{.CameraName}} saw a cat!",
                "embeds": [{ "title": "{{.Timestamp}}",
                             "image": { "url": "{{.ImageURL}}" } }] }
}
```

**Mechanism** (`notify.go`):
- Track `lastNotifiedEventID` alongside `lastEventTS`. When `fetchLatestEvent`
  observes an event ID newer than `lastNotifiedEventID`, render `template` with
  `text/template` and `POST` to `webhook_url` in a goroutine (non-blocking);
  update `lastNotifiedEventID`.
- **Template fields** (renamed from the `get_events` shape): `CameraName` ←
  `camera`, `EventID` ← `event_id`, `Timestamp` ← `time`/`event_ts`, `ImageURL`
  ← `thumbnail_url`, `VideoURL` ← `video_url`, `Tags` ← `tags`. Requires widening
  `fetchLatestEvent` to carry `thumbnail_url` — today it extracts only `event_ts`
  + `event_id` ([:243-262](../internal/viammod/conditional_camera.go#L243-L262)).
- **Never** notify from the `Image`/frame path (capture-cadence → spam).
  Edge-only. Failures log-and-drop; never block gating.

## Discovery service (Phase 5 design, optional)

Additive, **not** a replacement for the manager (which stays long-running: auth
session + go2rtc + RTSP). Discovery is a one-shot config generator.

- Model `cheukt:wyze-bridge:discovery`, `rdk:service:discovery`, same module.
  API confirmed in the pinned rdk (v0.132.0):
  `DiscoverResources(ctx, extra) ([]resource.Config, error)`.
- Depends on `manager` by name; `DiscoverResources` calls the manager's
  `list_cameras` with **`probe=false`** (reuses the live session — no second Wyze
  auth) and returns one `viam:viamrtsp:rtsp` config per camera, using each
  camera's reported `rtsp_url` **verbatim** — don't reconstruct the URL. The
  `probe=false` path returns a `rtsp_url` for every camera, including ones that
  haven't produced media yet, so offline cams are still addable and just work
  once they come online. Skip and log any camera missing a `rtsp_url`.
- **Emits plain rtsp cameras only** — no gated-camera configs. Gating stays a
  deliberate choice the user layers on afterward (add a `conditional-camera`
  pointing at a discovered rtsp cam + the manager); discovery doesn't synthesize
  it. Keeps every emitted config independent, not inter-dependent pairs.
- Turns "hand-write a camera per Wyze cam" into "click Discover."

## Optional future — motion as a standard surface

Not part of this plan; recorded so the door stays open. To serve public users who
prefer their own gate (or `filtered_camera`), the manager could expose Wyze
motion as a standard Viam surface — a `rdk:service:vision` returning a
`motion`/`cat` classification when a recent event exists, or a `sensor` whose
`Readings()` include `{event_active, event_id, event_ts}`. Additive; doesn't touch
our own gated camera. Defer until a second consumer asks.

## Open questions

- **Decided:** two repos not three; the split is worth it (public
  Wyze-ecosystem audience); keep our own gated camera (not `filtered_camera`).
- Namespace: keep `cheukt` for both module_ids and fix `meta.json` `url` to the
  real repo; leave the go.mod module path as upstream's (mergeability).
- cat-ui history: fresh repo (clean start) vs `git filter-repo` to preserve file
  history. Fresh is simpler; the files are small.
- Discovery: depends-on-manager (recommended, reuses session) vs standalone
  auth; build now or defer until a second consumer.
