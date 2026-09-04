package viammod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
)

// ConditionalModel is the resource model for the event-gated camera:
// cheukt:wyze-bridge:conditional-camera. It wraps an underlying camera and
// only passes frames through to data management when the wyze-bridge manager
// reports a recent motion event.
var ConditionalModel = resource.NewModel("cheukt", "wyze-bridge", "conditional-camera")

// errUnimplemented is returned by the camera surfaces this component doesn't
// support (point clouds, geometries).
var errUnimplemented = errors.New("unimplemented")

const (
	// defaultCondWindow is how recent an event must be for Images to pass
	// frames through. Matches the user-facing "event in the last 20s".
	defaultCondWindow = 20 * time.Second

	// defaultCondCooldown is Wyze's per-camera event cooldown: once an event
	// fires, no new event can occur for this long, so the poll loop stops
	// asking the manager until it elapses.
	defaultCondCooldown = 5 * time.Minute

	// defaultCondPoll is how often the background loop polls the manager for
	// events (when it isn't skipping the poll).
	defaultCondPoll = 1 * time.Second

	// maxRecentEvents caps the get_recent_events ring. An entry costs at least
	// one cooldown (maybePoll skips inside it), so at the default this is ~80
	// minutes of edges — enough that the command is worth running by hand.
	maxRecentEvents = 16
)

func init() {
	resource.RegisterComponent(camera.API, ConditionalModel,
		resource.Registration[camera.Camera, *ConditionalConfig]{Constructor: newConditionalCamera})
}

// ConditionalConfig is the JSON configuration for the conditional-camera
// component.
type ConditionalConfig struct {
	// Camera is the underlying camera resource name whose frames are gated.
	// Required. Typically the viam:viamrtsp:rtsp camera fed by this module's
	// manager.
	Camera string `json:"camera"`

	// Manager is the cheukt:wyze-bridge:manager generic service name to poll
	// for events. Required.
	Manager string `json:"manager"`

	// CameraName scopes which events count, matched against each event's
	// friendly name / nickname (case-insensitive). Optional; empty means any
	// event from any camera the manager reports triggers this component.
	CameraName string `json:"camera_name,omitempty"`

	// WindowSeconds is how recent an event must be for frames to pass through.
	// Optional; defaults to 20.
	WindowSeconds int `json:"window_seconds,omitempty"`

	// CooldownSeconds is the Wyze per-camera event cooldown; the poll loop
	// stops polling for this long after an event, since no new event is
	// possible. Optional; defaults to 300 (5 minutes).
	CooldownSeconds int `json:"cooldown_seconds,omitempty"`

	// PollSeconds is the background poll cadence. Optional; defaults to 1.
	PollSeconds float64 `json:"poll_seconds,omitempty"`

	// Debug enables verbose per-poll logging. Optional.
	Debug bool `json:"debug,omitempty"`

	// Stamp controls whether captured data-management frames are stamped with
	// the active event id (as a classification + full-frame bounding box) so
	// uploaded images are groupable by event. Optional; an absent block means no
	// stamping — the component stays a generic gate. A consumer that browses
	// captures per event (e.g. a dashboard) enables this and reads the labels
	// back with boundingBoxLabelsByFilter.
	Stamp *StampConfig `json:"stamp,omitempty"`
}

// StampConfig configures event-id stamping on captured data-management frames.
type StampConfig struct {
	// Enabled turns stamping on. Optional; false or an absent block means frames
	// pass through unstamped.
	Enabled bool `json:"enabled,omitempty"`

	// LabelPrefix is prepended to the event id to form the classification +
	// full-frame bounding-box label (e.g. "wyze_event:" -> "wyze_event:<id>").
	// Optional; defaults to "wyze_event:" when stamping is enabled.
	LabelPrefix string `json:"label_prefix,omitempty"`
}

// Validate enforces the two required dependencies and non-negative timings,
// and reports the camera + manager as required deps.
func (c *ConditionalConfig) Validate(path string) (requiredDeps, optionalDeps []string, err error) {
	if strings.TrimSpace(c.Camera) == "" {
		return nil, nil, fmt.Errorf(`%s: "camera" is required`, path)
	}
	if strings.TrimSpace(c.Manager) == "" {
		return nil, nil, fmt.Errorf(`%s: "manager" is required`, path)
	}
	if c.WindowSeconds < 0 || c.CooldownSeconds < 0 || c.PollSeconds < 0 {
		return nil, nil, fmt.Errorf(`%s: window_seconds, cooldown_seconds, and poll_seconds must not be negative`, path)
	}
	return []string{c.Camera, c.Manager}, nil, nil
}

// conditionalCamera is the running component. It embeds resource.Named
// (Name/DoCommand/Status) and resource.AlwaysRebuild (Reconfigure); Close is
// implemented below to stop the poll goroutine.
type conditionalCamera struct {
	resource.Named
	resource.AlwaysRebuild

	cam     camera.Camera
	manager resource.Resource
	logger  logging.Logger

	camName  string
	window   time.Duration
	cooldown time.Duration
	poll     time.Duration
	debug    bool

	// stampEnabled and stampPrefix are resolved from StampConfig at construction.
	// When stampEnabled is false, captured frames are not stamped.
	stampEnabled bool
	stampPrefix  string

	mu sync.Mutex
	// lastEventTS is the Wyze event_ts (motion time) of the most recent
	// matching event we've observed. Both the poll gate and the Images
	// condition are computed from this.
	lastEventTS time.Time
	// lastEventID is the Wyze event_id of that same event, attached to
	// data-management captures as a classification label so uploaded images
	// carry the event they were captured for. May be empty if the event had no
	// id.
	lastEventID string
	// recent is a bounded ring of the shaped events behind each detected edge,
	// served by get_recent_events; recentIdx is the next slot to write, which
	// once full is also the oldest entry. Entries must stay immutable after
	// append — that is what lets DoCommand hand the maps out and marshal them
	// with the mutex released.
	recent    []map[string]interface{}
	recentIdx int

	cancel context.CancelFunc
	done   chan struct{}
}

func newConditionalCamera(
	ctx context.Context,
	deps resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (camera.Camera, error) {
	newConf, err := resource.NativeConfig[*ConditionalConfig](conf)
	if err != nil {
		return nil, err
	}

	cam, err := camera.FromProvider(deps, newConf.Camera)
	if err != nil {
		return nil, err
	}
	mgr, err := resource.FromProvider[resource.Resource](deps, generic.Named(newConf.Manager))
	if err != nil {
		return nil, err
	}

	stampEnabled, stampPrefix := resolveStamp(newConf.Stamp)

	loopCtx, cancel := context.WithCancel(context.Background())
	cc := &conditionalCamera{
		Named:        conf.ResourceName().AsNamed(),
		cam:          cam,
		manager:      mgr,
		logger:       logger,
		camName:      strings.TrimSpace(newConf.CameraName),
		window:       secondsOr(newConf.WindowSeconds, defaultCondWindow),
		cooldown:     secondsOr(newConf.CooldownSeconds, defaultCondCooldown),
		poll:         pollOr(newConf.PollSeconds, defaultCondPoll),
		debug:        newConf.Debug,
		stampEnabled: stampEnabled,
		stampPrefix:  stampPrefix,
		cancel:       cancel,
		done:         make(chan struct{}),
	}

	go cc.pollLoop(loopCtx)
	return cc, nil
}

// pollLoop refreshes lastEventTS from the manager on a ticker, skipping the
// call whenever an event is recent enough that a poll can't change the answer.
func (cc *conditionalCamera) pollLoop(ctx context.Context) {
	defer close(cc.done)

	ticker := time.NewTicker(cc.poll)
	defer ticker.Stop()

	// Poll once up front so we don't wait a full interval before reacting.
	cc.maybePoll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cc.maybePoll(ctx)
		}
	}
}

// maybePoll decides whether to actually call the manager. It skips while the
// last event is younger than the cooldown, which subsumes both cases:
//   - age < window   : the condition is already true, no need to ask.
//   - age < cooldown : Wyze's cooldown means no new event is possible yet.
//
// Only once age >= cooldown (or we've never seen an event) does it poll.
func (cc *conditionalCamera) maybePoll(ctx context.Context) {
	cc.mu.Lock()
	last := cc.lastEventTS
	cc.mu.Unlock()

	if !last.IsZero() && time.Since(last) < cc.cooldown {
		if cc.debug {
			cc.logger.Debugw("conditional camera: skipping poll",
				"age", time.Since(last).String(), "cooldown", cc.cooldown.String())
		}
		return
	}

	ts, event, ok := cc.fetchLatestEvent(ctx)
	if !ok {
		return
	}
	id, _ := event["event_id"].(string)
	cc.mu.Lock()
	if ts.After(cc.lastEventTS) {
		cc.lastEventTS = ts
		cc.lastEventID = id
		cc.noteRecent(event)
	}
	cc.mu.Unlock()
	if cc.debug {
		cc.logger.Debugw("conditional camera: event observed",
			"event_ts", ts.UTC().Format(time.RFC3339), "event_id", id)
	}
}

// fetchLatestEvent asks the manager for events within the window and returns
// the newest matching event's timestamp alongside the whole shaped event, if
// any. The full event (not just its timestamp and id) is what feeds the
// get_recent_events ring, which serves fields the gate itself never reads —
// nickname, tags, thumbnail_url.
func (cc *conditionalCamera) fetchLatestEvent(ctx context.Context) (time.Time, map[string]interface{}, bool) {
	resp, err := cc.manager.DoCommand(ctx, map[string]interface{}{
		"get_events": map[string]interface{}{"window_seconds": int(cc.window.Seconds())},
	})
	if err != nil {
		cc.logger.Debugw("conditional camera: get_events failed", "error", err.Error())
		return time.Time{}, nil, false
	}

	raw, ok := resp["events"].([]interface{})
	if !ok {
		return time.Time{}, nil, false
	}

	var newest time.Time
	var newestEvent map[string]interface{}
	for _, item := range raw {
		e, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if cc.camName != "" && !cc.matchesCamera(e) {
			continue
		}
		ts, ok := asInt(e["event_ts"])
		if !ok || ts <= 0 {
			continue
		}
		if t := time.UnixMilli(ts); t.After(newest) {
			newest = t
			newestEvent = e
		}
	}
	if newest.IsZero() {
		return time.Time{}, nil, false
	}
	return newest, newestEvent, true
}

// noteRecent appends a shaped event to the ring, evicting the oldest once it is
// full. Caller holds cc.mu.
func (cc *conditionalCamera) noteRecent(e map[string]interface{}) {
	if len(cc.recent) < maxRecentEvents {
		cc.recent = append(cc.recent, e)
		return
	}
	cc.recent[cc.recentIdx] = e
	cc.recentIdx = (cc.recentIdx + 1) % maxRecentEvents
}

// recentEvents copies the ring out oldest-first. The slice is fresh; the event
// maps are shared, which is safe because they are never mutated after append.
func (cc *conditionalCamera) recentEvents() []interface{} {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	n := len(cc.recent)
	out := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, cc.recent[(cc.recentIdx+i)%n])
	}
	return out
}

// matchesCamera reports whether a shaped event belongs to the configured
// camera, comparing CameraName against the event's "camera" (stream name) and
// "nickname" fields case-insensitively.
func (cc *conditionalCamera) matchesCamera(e map[string]interface{}) bool {
	want := strings.ToLower(cc.camName)
	for _, k := range []string{"camera", "nickname"} {
		if s, ok := e[k].(string); ok && strings.ToLower(strings.TrimSpace(s)) == want {
			return true
		}
	}
	return false
}

// eventActive reports whether the most recent matching event is within the
// window — the condition under which frames are passed through — and returns
// that event's id so it can be stamped onto captured frames.
func (cc *conditionalCamera) eventActive() (id string, active bool) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if cc.lastEventTS.IsZero() || time.Since(cc.lastEventTS) >= cc.window {
		return "", false
	}
	return cc.lastEventID, true
}

// Images returns the underlying camera's frames when the call is a live view
// (always) or when it is a data-management capture and an event is active.
// Data-management captures with no active event are dropped with
// data.ErrNoCaptureToStore so nothing is stored.
func (cc *conditionalCamera) Images(
	ctx context.Context,
	filterSourceNames []string,
	extra map[string]interface{},
) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	images, meta, err := cc.cam.Images(ctx, filterSourceNames, extra)
	if err != nil {
		return images, meta, err
	}

	// Live viewers always get frames; only gate data-management captures.
	if !isFromDataMgmt(extra) {
		return images, meta, nil
	}

	id, active := cc.eventActive()
	if !active {
		return nil, meta, data.ErrNoCaptureToStore
	}
	// When stamping is enabled, stamp the event id onto each frame so the
	// uploaded image carries the Wyze event it was captured for. Flows through
	// the camera collector's Binary.Annotations to cloud data.
	if cc.stampEnabled && id != "" {
		images = stampEventID(images, cc.stampPrefix+id)
	}
	return images, meta, nil
}

// defaultEventClassPrefix is the label namespace used when stamping is enabled
// but no label_prefix is configured. It marks the classification / bounding box
// as a captured Wyze event, distinguishable from real ML labels.
const defaultEventClassPrefix = "wyze_event:"

// resolveStamp derives the stamping on/off flag and label prefix from config.
// A nil block or Enabled=false disables stamping; an enabled block with no
// LabelPrefix falls back to defaultEventClassPrefix.
func resolveStamp(sc *StampConfig) (enabled bool, prefix string) {
	if sc == nil || !sc.Enabled {
		return false, ""
	}
	if prefix = strings.TrimSpace(sc.LabelPrefix); prefix == "" {
		prefix = defaultEventClassPrefix
	}
	return true, prefix
}

// stampEventID returns images with the given label added to each frame's
// annotations, as both a classification and a full-frame bounding box. The
// classification marks the frame as a captured Wyze event; the bounding box
// makes the event id server-filterable in cloud data (binaryDataByFilter's
// bbox_labels, boundingBoxLabelsByFilter) since the data API can't filter on
// classification labels. It copies each NamedImage's annotation slices so the
// underlying camera's slices aren't mutated.
func stampEventID(images []camera.NamedImage, label string) []camera.NamedImage {
	for i := range images {
		ann := images[i].Annotations

		classes := make([]data.Classification, len(ann.Classifications), len(ann.Classifications)+1)
		copy(classes, ann.Classifications)
		ann.Classifications = append(classes, data.Classification{Label: label})

		boxes := make([]data.BoundingBox, len(ann.BoundingBoxes), len(ann.BoundingBoxes)+1)
		copy(boxes, ann.BoundingBoxes)
		ann.BoundingBoxes = append(boxes, data.BoundingBox{
			Label:          label,
			XMinNormalized: 0, YMinNormalized: 0,
			XMaxNormalized: 1, YMaxNormalized: 1,
		})

		images[i].Annotations = ann
	}
	return images
}

// NextPointCloud is unsupported.
func (cc *conditionalCamera) NextPointCloud(ctx context.Context, extra map[string]interface{}) (pointcloud.PointCloud, error) {
	return nil, errUnimplemented
}

// Properties passes through the underlying camera's properties, clearing PCD
// support since this component doesn't serve point clouds.
func (cc *conditionalCamera) Properties(ctx context.Context) (camera.Properties, error) {
	p, err := cc.cam.Properties(ctx)
	if err == nil {
		p.SupportsPCD = false
	}
	return p, err
}

// Geometries is unsupported.
func (cc *conditionalCamera) Geometries(ctx context.Context, extra map[string]interface{}) ([]spatialmath.Geometry, error) {
	return nil, errUnimplemented
}

// DoCommand serves one command:
//
//	{"get_recent_events": {}}  ->  {"events": [<shaped event>, ...]}
//
// The whole ring, oldest-first. Two things a caller must design around:
//
// It is a feed of detected *edges*, not an event history. The poll loop skips
// while inside the cooldown and asks for only window_seconds at a time, keeping
// the single newest match, so at most one entry lands per cooldown and an event
// arriving between those two marks is never seen.
//
// That rate is per component, not per camera, because the cooldown gate is one
// component-wide watermark. A component with camera_name set therefore sees its
// own camera's edges; an unscoped one rings whatever the manager reports and
// still appends only once per cooldown across every camera, so it drops most of
// them and is not a usable per-camera feed. Two unscoped components also return
// the same events, so a caller polling several must dedupe across all of them.
//
// There is deliberately no since/limit argument. See DOCS/VIAM_MODULE.md.
func (cc *conditionalCamera) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if _, ok := cmd["get_recent_events"]; ok {
		return map[string]interface{}{"events": cc.recentEvents()}, nil
	}
	return nil, resource.ErrDoUnimplemented
}

// Close stops the poll goroutine and waits for it to exit. Idempotent via the
// cancelled context (a second call sees the goroutine already gone).
func (cc *conditionalCamera) Close(ctx context.Context) error {
	cc.cancel()
	select {
	case <-cc.done:
	case <-ctx.Done():
	}
	return nil
}

// isFromDataMgmt reports whether an Images call originated from data
// management (as opposed to a live client), from the extra map.
func isFromDataMgmt(extra map[string]interface{}) bool {
	if extra == nil {
		return false
	}
	v, ok := extra[data.FromDMString].(bool)
	return ok && v
}

// secondsOr converts a whole-seconds config value to a Duration, falling back
// to def when unset (<= 0).
func secondsOr(secs int, def time.Duration) time.Duration {
	if secs <= 0 {
		return def
	}
	return time.Duration(secs) * time.Second
}

// pollOr converts a fractional-seconds poll cadence to a Duration, falling
// back to def when unset (<= 0).
func pollOr(secs float64, def time.Duration) time.Duration {
	if secs <= 0 {
		return def
	}
	return time.Duration(secs * float64(time.Second))
}
