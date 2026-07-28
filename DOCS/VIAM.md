# Wyze Bridge — Viam Module

The `cheukt:wyze-bridge` Viam module exposes Wyze cameras to a Viam machine.

## Models

- **`cheukt:wyze-bridge:manager`** (`rdk:service:generic`) — embeds the wyze core
  (auth + camera discovery + embedded go2rtc) and publishes each camera as a
  loopback RTSP stream (`rtsp://127.0.0.1:8554/<name>`) for `viam:viamrtsp:rtsp`
  cameras to consume. Exposes camera status and motion events over `DoCommand`
  (`list_cameras`, `get_events`).
- **`cheukt:wyze-bridge:conditional-camera`** (`rdk:component:camera`) — wraps an
  underlying camera and gates its data-management captures on recent Wyze motion
  events reported by the manager.

The LAN-local dashboard (camera video + a browsable history of uploaded
motion-event images) now ships as a separate module, `cheukt:home:home-ui`.

See [VIAM_MODULE_PLAN.md](VIAM_MODULE_PLAN.md) for the design and
[MODULE_SPLIT_PLAN.md](MODULE_SPLIT_PLAN.md) for the module split.

## Changes

Breaking and notable behavior changes for the Viam module. These affect only your
own machines' configs.

### `conditional-camera` stamping is now opt-in

The `conditional-camera` component used to **always** stamp each captured
data-management frame with the active Wyze event id — a `wyze_event:<id>`
classification plus a full-frame bounding box — so uploaded images are groupable
by event. Stamping is now **opt-in and off by default**, so the component stays a
generic gate unless a consumer needs the labels.

**To preserve the previous behavior** — required if you use the `cheukt:home:home-ui`
dashboard, which browses uploads by these labels — add a `stamp` block to the
component's config:

```json
{
  "stamp": { "enabled": true }
}
```

`label_prefix` optionally overrides the default `wyze_event:` namespace (the
camera and any consumer must agree on it):

```json
{
  "stamp": { "enabled": true, "label_prefix": "cat_event:" }
}
```

With no `stamp` block, gating still works exactly as before — only the event-id
labels on captured frames are omitted.
