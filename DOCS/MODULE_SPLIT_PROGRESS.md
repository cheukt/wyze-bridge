# Module Split — Progress & Handoff

Session handoff for the module-split / public-release effort. Read this first,
then [MODULE_SPLIT_PLAN.md](MODULE_SPLIT_PLAN.md) for the full design.

_Last updated: 2026-07-27. This repo: branch `viam-module`, HEAD `258ffc9` with
**uncommitted Phase 2 changes** (cat-ui source deleted — see Phase 2 below).
Extracted repo: `repos/home` (`github.com/cheukt/home`), `main` at `3a45f43`,
pushed and in sync with origin._

## The effort in one paragraph

Split the single `cheukt:wyze-bridge` Viam module so the **manager** (+ gated
camera + optional discovery) can be published **publicly** for the Viam
ecosystem, while the bespoke **cat-ui** dashboard moves to its own **private**
module. This is a **two-repo** plan (not three — see decisions). The repo also
becomes **Viam-only in identity/release** while staying a **cleanly mergeable
fork** of the Go upstream `IDisposable/docker-wyze-bridge`.

## Locked decisions (do NOT relitigate)

1. **Two repos, not three.** Public `cheukt:wyze-bridge` = manager +
   `conditional-camera` (gated camera) + optional `discovery`. Private
   `cheukt:home` (originally planned as `cheukt:cat-ui`) = the dashboard, model
   `cheukt:home:home-ui`. The gated camera stays with the manager (its
   event source), which keeps the `get_events` contract internal.
2. **Keep our own gated camera** — not `viam-modules`/`erh/filtered_camera`
   (bloated; our event source is Wyze cloud motion via `DoCommand`, not a vision
   service).
3. **Viam-only = identity/release, not deletion.** Don't delete upstream Docker
   files (`docker/`, `docs/`, `docker-compose.yml`, `setup.sh`, `MIGRATION.md`) —
   deleting them creates permanent merge conflicts. Just stop publishing the
   Docker image and lead the README with Viam.
4. **Do NOT rename the go.mod module path.** Keep
   `module github.com/IDisposable/docker-wyze-bridge` (imported by 44 files;
   renaming collides with every upstream merge for no gain — a Go module path
   needn't match the repo host). Fix only `meta.json` `url`.
5. **Keep Viam changes out of upstream-owned files.** Viam user docs live in
   [DOCS/VIAM.md](VIAM.md) (add-only path), NOT `MIGRATION.md`. Use
   `.gitattributes merge=ours` for the few shared-path files we own (README.md,
   maybe Makefile). Keep edits out of the core (`internal/wyzeapi`,
   `internal/camera`, `internal/go2rtcmgr`) — wrap in `viammod` instead.
6. **cat-ui's cross-repo seam is `list_cameras` + the `wyze_event:` label
   schema.** `get_events` is internal (gated camera + notify only). No
   `contract_version` ceremony.
7. **Discovery emits plain `viam:viamrtsp:rtsp` cameras only** (no gated-camera
   pairs), using `list_cameras probe=false` reported `rtsp_url` verbatim.

## Done

- **Phase 0 — opt-in stamping — COMPLETE & COMMITTED** (`c200938`).
  - `conditional-camera` stamping (`wyze_event:<id>` classification + full-frame
    bbox) is now **opt-in via a `stamp` config block** (`{enabled, label_prefix}`),
    **default OFF**. Files: [conditional_camera.go](../internal/viammod/conditional_camera.go)
    (added `StampConfig`, `resolveStamp` helper, gated the stamp call, renamed
    const `eventClassPrefix` → `defaultEventClassPrefix`) + its test.
  - Behavior change documented in [DOCS/VIAM.md](VIAM.md) (created this effort).
  - Full design captured in [MODULE_SPLIT_PLAN.md](MODULE_SPLIT_PLAN.md) (also
    created/committed this effort).
  - `go build ./...`, `go vet`, `go test ./...` all green.
- **Phase 1 readiness — VERIFIED READY** (checks below already run):
  - Working tree clean; branch green.
  - cat-ui Go references **zero** core symbols (calls, types, vars, interfaces
    all checked).
  - cat-ui `.go` files import **no** `internal/` packages (rdk-only: logging,
    resource, generic, utils).
  - Movable artifacts present: `internal/viammod/catui{,_http,_dev,_test}.go`,
    `internal/viammod/ui/` (incl. built `ui/dist`), `cmd/catui-dev/`.

## ⚠️ Operational reminder (user's machine, not a repo action)

The stamping default flipped to **off**. The live cat-ui machine config must add
`"stamp": {"enabled": true}` to its gated-camera component **before this reaches
that machine**, or uploaded frames silently stop being tagged and cat-ui's event
history breaks.

## Phase 1 — extract cat-ui to a private repo — DONE (published)

**Naming changed twice vs. the original plan.** Final: the private repo is
**`cheukt/home`** (cloned to `repos/home`) — not `cheukt/cat-ui`, and no longer
`cheukt/home-ui` either. The module family was widened to `home` so it can hold
more than the dashboard later, with the dashboard as one model inside it:
`cheukt:wyze-bridge:cat-ui` → `cheukt:home-ui:home-ui` → **`cheukt:home:home-ui`**
(`rdk:service:generic`; user decision, 2026-07-27, commit `fb3b4b8`). The Go
package is still `homeui/` and the dev command is still `cmd/homeui-dev` — only
the module path and registry identity moved. The Svelte UI's *display* title
("🐱 Cat Cam") was intentionally left unchanged.

Done (committed at `repos/home` HEAD `3a45f43`, on `main`, **pushed**):

- `go.mod` → `module github.com/cheukt/home`; `go 1.26.2`; **rdk `v1.0.0`**.
  ⚠️ This diverges from the parent repo, which is still on `v0.132.0` — the
  earlier "repin to match the parent" note no longer holds. The split repo is
  green on `v1.0.0` in CI; the seam is DoCommand JSON, not shared Go types, so
  the version skew is tolerable — but if the parent later upgrades, re-check
  nothing in `homeui/` depended on the older rdk API.
- `cmd/module/main.go` registers `homeui.Model` (`rdk:service:generic`) as
  `cheukt:home:home-ui`.
- `homeui/{homeui,homeui_http,homeui_dev,homeui_test}.go` — was
  `internal/viammod/catui*.go`. Symbols renamed: `CatUIModel`→`Model`,
  `CatUIConfig`→`Config`, `catUIService`→`service`, `newCatUI`→`newService`;
  `UIConfig`, `ServeUI` kept (exported surface `cmd/homeui-dev` uses).
- `homeui/ui/` — Svelte source moved adjacent to `homeui_http.go` (the
  `//go:embed all:ui/dist` relative path). `dist/.gitkeep` placeholder kept.
- `cmd/homeui-dev/main.go` — was `cmd/catui-dev`; import repointed to
  `github.com/cheukt/home/homeui`. Env prefix `CATUI_*`→`HOMEUI_*`.
- `meta.json` → `module_id cheukt:home`, `url github.com/cheukt/home`, model
  `cheukt:home:home-ui`, `visibility private`, entrypoint `bin/module`.
  **No go2rtc** in the bundle (a pure static server) — `module.tar.gz` =
  `bin/module` + `meta.json`.
- `Makefile` (`ui`/`module`/`module.tar.gz`/`run-ui`/`reload`/`test`/`lint`),
  `setup.sh` (Node 20), `.golangci.yml`, `.gitignore`, `.env.viam.example`,
  `README.md` (documents the `list_cameras` shape + `wyze_event:` label seam).
- **CI/CD** (added 2026-07-24, trimmed `3a45f43`) — three workflows:
  - `checks.yml` — reusable `workflow_call`: lint (golangci-lint v2.11.4),
    test, and a full build (Node 20 + `make ui`, then `go build ./...`).
    Defined once so the PR checks and the deploy gate cannot drift.
  - `ci.yml` — PRs to `main` only; just calls `checks.yml`. Pushes to `main`
    deliberately don't run it (deploy.yml already runs the same checks).
  - `deploy.yml` — on push to `main` (paths-ignore `**/*.md`, `docs/**`),
    on published release, or manual dispatch. Gates on `checks.yml`, bumps an
    `rc` prerelease tag (`mathieudutour/github-tag-action`, `main` = prerelease
    branch, release tags come only from the release event), then publishes via
    `viamrobotics/build-action` (Viam cloud build runs `make
    module.tar.gz` per arch). Serialized by a `concurrency: deploy` group with
    `cancel-in-progress: false` — a cancelled run could tag without uploading.

Verified: `go build/vet/test ./...` green; `make ui` builds the SPA; the embed
serves the real `index.html` (mux test 200); `make module.tar.gz` produces a
valid bundle; CI green on `main`.

**Published.** The namespace blocker is resolved — `cheukt` is claimed and
`cheukt:home` is registered **private** in the Viam registry. Cloud builds are
green on all three arches (`linux/amd64`, `linux/arm64`, `darwin/arm64`) for
`0.0.1-rc.0` → `rc.2`; `rc.3` was building at the time of this update. Check
with `viam module build list`.

Publishing is now **automatic**: push to `main` → `checks.yml` gate → rc tag
bump → cloud build + upload. No manual `viam module create/update/build start`
runbook is needed anymore (the old blocked runbook has been removed from this
doc). A real release version comes from publishing a GitHub release (the tag
becomes the version), or `workflow_dispatch` with an explicit version.

**Not yet done (needs the user):**
- `make run-ui` against a live machine — needs `.env.viam` (real machine
  creds); still only compile-verified.
- Machine still runs old `cheukt:wyze-bridge:cat-ui` — nothing broken yet, but
  the cutover (Phase 2) hasn't started.
- Cut a non-rc version when ready (GitHub release) rather than staying on
  `0.0.1-rc.N`.

CLI is authenticated as `cheuk@viam.com` (viam v0.131.0 locally — the CLI now
warns it's out of date vs. v1.0.0).

## Phase 2 — slim this repo + cut over — DONE (source removed)

The live machine was cut over to `cheukt:home:home-ui` (confirmed running by the
user), so the cat-ui source has been removed from this repo:

- **Deleted:** `internal/viammod/catui{,_dev,_http,_test}.go`,
  `internal/viammod/ui/` (whole Svelte tree), `cmd/catui-dev/`.
- **`cmd/viam-module/main.go`** no longer registers `viammod.CatUIModel` (that
  symbol is gone) — only `manager` + `conditional-camera` remain.
- **`meta.json`** dropped the `cat-ui` model entry (2 models now).
- **`Makefile`** dropped the `ui` / `run-ui` targets, `UI_DIR`, and the
  `bin/viam-module: ui` dependency. The module is now a **pure Go build** — no
  `//go:embed all:ui/dist`, no Node toolchain. `setup.sh` has since been
  removed entirely (no frontend to provision, so no setup step) and the
  `build.setup` key dropped from `meta.json`.
- **Docs/config swept:** `.gitignore` (dropped the Svelte section),
  `.env.viam.example` (trimmed to just `VIAM_PART_ID` for `make reload`),
  `DOCS/VIAM.md`, `cmd/viam-module/README.md`, `DEVELOPER.md`, `CLAUDE.md`
  package map, and a "historical" banner on `DOCS/VIAM_MODULE_PLAN.md`. All now
  point the dashboard at `cheukt:home:home-ui`.
- **Verified:** `go build/vet/test ./...` green; `make module.tar.gz` produces a
  valid `bin/viam-module + bin/go2rtc + meta.json` bundle (no Node step).

**Not yet done:** commit + push these changes, then re-publish (push to `main`
triggers the cloud build). This progress file is still untracked here — commit
or discard alongside.

## Remaining phases (summary — see plan for detail)

- **Phase 3 — go public** (only after cat-ui source is gone; the `cheukt`
  namespace claim that this also needed is now done): make GitHub repo
  public, set `meta.json` visibility public + `url` → `github.com/cheukt/wyze-bridge`
  (leave go.mod path), Viam-first README (preserve AGPL attribution +
  THIRD_PARTY_NOTICES), `.gitattributes merge=ours`, add upstream remote.
- **Phase 4 — notify** (independent): `notify.go` in the gated camera; edge-only
  Discord/webhook via `text/template`; widen `fetchLatestEvent` to carry
  `thumbnail_url`.
- **Phase 5 — discovery** (optional): `rdk:service:discovery`; plain rtsp cams
  from `list_cameras probe=false`.

## Uncommitted state

This repo has **uncommitted Phase 2 changes** (cat-ui deletion + doc sweep, ~27
files) plus this untracked progress file — not yet committed or pushed. In
`repos/home`, `main` is clean and in sync with origin.

## Known staleness elsewhere

[MODULE_SPLIT_PLAN.md](MODULE_SPLIT_PLAN.md) still describes the extracted repo
throughout as `cheukt:cat-ui` / `github.com/cheukt/cat-ui` / package `catui/`.
That naming is superseded by `cheukt:home` / `github.com/cheukt/home` / package
`homeui/` (see Phase 1 above). The plan's *design* — the seam, the phase order,
the locked decisions — is still accurate; only the names are outdated.
