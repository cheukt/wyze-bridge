// Package viammod exposes the wyze-headless core as a Viam generic service:
// cheukt:wyze-bridge:manager. It embeds Wyze auth + camera discovery +
// embedded go2rtc in-process and publishes each camera as a loopback RTSP
// stream that standard viam:viamrtsp:rtsp camera components consume. Wyze
// credentials are read from an on-machine creds_file, never from the Viam
// cloud config. See DOCS/VIAM_MODULE.md.
package viammod

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/go2rtcmgr"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// Version is reported to the Wyze API as the bridge version. Overridable at
// build time via -ldflags "-X .../internal/viammod.Version=...".
var Version = "viam-module"

// Model is the resource model for this service: cheukt:wyze-bridge:manager.
var Model = resource.NewModel("cheukt", "wyze-bridge", "manager")

func init() {
	resource.RegisterService(generic.API, Model,
		resource.Registration[resource.Resource, *Config]{Constructor: newService})
}

// Validate enforces only that creds_file is set and returns the service's
// dependencies (none — it is self-contained). The creds file contents are
// validated at construction time, since Validate may run before the file is
// present on the host.
func (c *Config) Validate(path string) (requiredDeps, optionalDeps []string, err error) {
	deps, err := c.validate(path)
	return deps, nil, err
}

// service is the running cheukt:wyze-bridge:manager resource.
type service struct {
	resource.Named
	resource.AlwaysRebuild

	camMgr   *camera.Manager
	go2rtcM  *go2rtcmgr.Manager
	api      eventLister
	log      zerolog.Logger
	rtspPort int

	cancel context.CancelFunc

	closeOnce sync.Once
}

// newService builds the service. It ports the body of wyze-headless main():
// load creds from the on-machine file, build config, start go2rtc, wire the
// camera manager, and run the discovery loop in the background.
func newService(
	ctx context.Context,
	_ resource.Dependencies,
	conf resource.Config,
	logger logging.Logger,
) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*Config](conf)
	if err != nil {
		return nil, err
	}

	creds, err := loadCredsFile(cfg.CredsFile)
	if err != nil {
		return nil, err
	}

	rtspPort := resolveRTSPPort(cfg.RTSPPort)
	bridgeCfg := buildBridgeConfig(cfg)

	// zerolog -> Viam logger; everything passes through, Viam filters by level.
	zl := newZerologToViam(logger)
	// Capture zerolog's package global so call sites that log through it (e.g.
	// internal/wyzeapi/state.go) also reach the Viam logger. See logadapter.go.
	captureZerologGlobals(zl)

	// serviceCtx drives the discovery loop + go2rtc; Close cancels it.
	serviceCtx, cancel := context.WithCancel(context.Background())

	apiClient := wyzeapi.NewClient(creds, Version, zl.With().Str("c", "wyzeapi").Logger())
	camMgr := camera.NewManager(bridgeCfg, apiClient, nil, zl.With().Str("c", "camera").Logger())
	// Actively verify media so camera state reflects reality, not just go2rtc
	// stream registration (go2rtc connects sources lazily).
	camMgr.SetHealthProbe(true)

	go2rtcAPI, go2rtcM, err := setupGo2RTC(serviceCtx, bridgeCfg, rtspPort, zl.With().Str("c", "go2rtc").Logger())
	if err != nil {
		cancel()
		return nil, err
	}

	camMgr.SetGo2RTCAPI(go2rtcAPI)
	go camMgr.RunDiscoveryLoop(serviceCtx)

	logger.Infow("wyze-bridge manager started",
		"rtsp_port", rtspPort, "state_dir", bridgeCfg.StateDir)

	return &service{
		Named:    conf.ResourceName().AsNamed(),
		camMgr:   camMgr,
		go2rtcM:  go2rtcM,
		api:      apiClient,
		log:      zl.With().Str("c", "viammod").Logger(),
		rtspPort: rtspPort,
		cancel:   cancel,
	}, nil
}

// buildBridgeConfig maps the Viam Config onto the bridge's *config.Config,
// resolving defaults and carrying over the literals headless's loadConfig
// used for the TUTK + embedded-go2rtc core.
func buildBridgeConfig(c *Config) *config.Config {
	stun := c.STUNServer
	if stun == "" {
		stun = "stun:stun.l.google.com:19302"
	}

	return &config.Config{
		BridgeIP:        c.BridgeIP,
		StateDir:        resolveStateDir(c.StateDir),
		LogLevel:        config.ParseLogLevel(c.LogLevel),
		ForceIOTCDetail: c.ForceIOTCDetail,
		STUNServer:      stun,

		// Restrict which cameras are exposed as streams. Empty = expose all.
		// Normalized to the uppercased/trimmed form the camera Filter expects
		// (the env path does this in envList; JSON arrays arrive raw).
		FilterNames:  normalizeFilter(c.FilterNames),
		FilterModels: normalizeFilter(c.FilterModels),
		FilterMACs:   normalizeFilter(c.FilterMACs),
		FilterBlocks: c.FilterBlock,
		// An explicit filter here is an intentional allow-list, so a no-match
		// exposes nothing rather than falling back to every camera.
		FilterAllowEmpty: true,

		// Literals for the TUTK + embedded-go2rtc core (from headless loadConfig).
		RefreshInterval: 30 * time.Minute, // must be non-zero (ticker)
		Quality:         "hd",
		Audio:           true,
		BridgePort:      5080,

		// The module targets TUTK cameras only. Gwell (OG-family) needs the
		// gwell-proxy sidecar, and WebRTC/KVS (doorbell lineage) needs the
		// bridge's /internal/wyze/webrtc shim HTTP server — neither of which
		// the module runs. So the TUTK→WebRTC auto-fallback is deliberately
		// left off (threshold 0 disables recordTUTKFailure promotion): there
		// is no WebRTC path to promote to. See DOCS/VIAM_MODULE.md.
		GwellEnabled:          false,
		TUTKFallbackThreshold: 0,
		CamOverrides:          make(map[string]config.CamOverride),
	}
}

// normalizeFilter trims and uppercases each filter value, dropping blanks, to
// match the contract the camera Filter relies on (its matches() uppercases the
// camera fields and compares against the filter values verbatim). The env path
// does this in envList; JSON arrays from the Viam config arrive raw.
func normalizeFilter(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.ToUpper(strings.TrimSpace(s)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// resolveRTSPPort defaults the go2rtc RTSP port to 8554.
func resolveRTSPPort(port int) int {
	if port <= 0 {
		return 8554
	}
	return port
}

// resolveStateDir defaults StateDir to the Viam per-module data directory
// ($VIAM_MODULE_DATA), falling back to headless's ./local/config when neither
// an explicit value nor the env is set.
func resolveStateDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if md := os.Getenv("VIAM_MODULE_DATA"); md != "" {
		return md
	}
	return "./local/config"
}

// DoCommand is the discovery surface. Supported commands:
//   - {"list_cameras": true}                                  -> camera list + rtsp URLs
//   - {"get_events": true}                                    -> motion events in the last minute
//   - {"restart_camera": "<name>"}                            -> reconnect a stream
//   - {"set_quality": {"name": "<n>", "quality": "hd"|"sd"}}  -> change quality
func (s *service) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if v, ok := cmd["list_cameras"]; ok {
		return s.listCameras(ctx, listCamerasProbe(v)), nil
	}

	if v, ok := cmd["get_events"]; ok {
		return s.getEvents(ctx, eventWindow(v))
	}

	if v, ok := cmd["restart_camera"]; ok {
		name, ok := v.(string)
		if !ok || name == "" {
			return nil, fmt.Errorf(`"restart_camera" requires a camera name string`)
		}
		s.camMgr.RestartStream(ctx, name)
		return map[string]interface{}{"restarted": name}, nil
	}

	if v, ok := cmd["set_quality"]; ok {
		args, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf(`"set_quality" requires an object {"name":..,"quality":..}`)
		}
		name, _ := args["name"].(string)
		quality, _ := args["quality"].(string)
		if name == "" || quality == "" {
			return nil, fmt.Errorf(`"set_quality" requires non-empty "name" and "quality"`)
		}
		if err := s.camMgr.SetQuality(ctx, name, quality); err != nil {
			return nil, err
		}
		return map[string]interface{}{"name": name, "quality": quality}, nil
	}

	return nil, fmt.Errorf("unknown command; supported: list_cameras, get_events, restart_camera, set_quality")
}

// probeTimeout bounds a single camera probe. go2rtc must dial the camera
// (the source connects lazily) and decode a keyframe — its own discovery
// timeout is ~5s, so allow headroom.
const probeTimeout = 10 * time.Second

// maxConcurrentProbes bounds how many cameras listCameras probes at once, so a
// large fleet (or several overlapping list_cameras calls) doesn't fan out an
// unbounded burst of forced go2rtc dials.
const maxConcurrentProbes = 8

// listCameras returns the camera list with loopback RTSP URLs. When probe is
// true (the default) it actively verifies each camera by fetching a frame from
// go2rtc — which forces the lazy source to dial the camera — and reports
// honest readiness: `rtsp_url` is included only for cameras that actually
// produce media, and an `error` field carries go2rtc's reason (e.g. "discovery
// timeout") for those that don't. With probe=false it skips the probe and
// reports the manager's (optimistic) state for a fast, side-effect-free list.
func (s *service) listCameras(ctx context.Context, probe bool) map[string]interface{} {
	cams := s.camMgr.Cameras()
	list := make([]interface{}, len(cams))

	sem := make(chan struct{}, maxConcurrentProbes)
	var wg sync.WaitGroup
	for i, cam := range cams {
		i, cam := i, cam
		name := cam.Name()
		// Snapshot once under the camera lock rather than reading cam.Info /
		// cam.State directly — the discovery loop calls UpdateInfo concurrently.
		snap := cam.Snapshot()
		entry := map[string]interface{}{
			"name":     name,
			"nickname": snap.Info.Nickname,
			"model":    snap.Info.Model,
			"state":    snap.State.String(),
		}
		list[i] = entry

		if !probe {
			// Fast path: trust the manager's state, always emit the URL.
			entry["rtsp_url"] = s.rtspURL(name)
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			if err := s.camMgr.ProbeStream(pctx, name); err != nil {
				// Verified NOT live: drop the URL, surface why, and don't
				// let a stale "streaming" state read as usable.
				entry["ready"] = false
				entry["error"] = err.Error()
				if snap.State == camera.StateStreaming {
					entry["state"] = "error"
				}
				return
			}
			entry["ready"] = true
			entry["state"] = "streaming"
			entry["rtsp_url"] = s.rtspURL(name)
		}()
	}
	wg.Wait()

	return map[string]interface{}{"cameras": list}
}

// rtspURL builds the loopback RTSP URL for a stream name.
func (s *service) rtspURL(name string) string {
	return fmt.Sprintf("rtsp://127.0.0.1:%d/%s", s.rtspPort, name)
}

// listCamerasProbe decides whether to actively probe, from the list_cameras
// argument. Accepts a bare truthy value (`{"list_cameras": true}` → probe) or
// an object `{"list_cameras": {"probe": false}}` to opt out. Defaults to
// probing so the list reflects reality.
func listCamerasProbe(v interface{}) bool {
	if obj, ok := v.(map[string]interface{}); ok {
		if p, ok := obj["probe"].(bool); ok {
			return p
		}
	}
	return true
}

// Close cancels the service context and stops go2rtc. Idempotent.
func (s *service) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.go2rtcM != nil {
			_ = s.go2rtcM.Stop()
		}
	})
	return nil
}
