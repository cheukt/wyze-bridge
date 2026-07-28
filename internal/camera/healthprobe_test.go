package camera

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/go2rtcmgr"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// probeGo2RTC is a fake go2rtc whose frame.mp4 and stream-producer behavior
// is controllable per stream name, to drive health-probe liveness outcomes.
type probeGo2RTC struct {
	mu sync.Mutex
	// live[name]=true => frame.mp4 returns an MP4 (camera produces media);
	// false/absent => 404 (e.g. discovery timeout).
	live map[string]bool
	// media[name]=true => /api/streams reports the producer carrying media
	// tracks (something is actively consuming), so the passive fast-path hits.
	media map[string]bool
}

func newProbeGo2RTC(t *testing.T) (*probeGo2RTC, *go2rtcmgr.APIClient) {
	t.Helper()
	p := &probeGo2RTC{live: map[string]bool{}, media: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		switch {
		case r.URL.Path == "/api/streams" && r.Method == "GET":
			result := map[string]*go2rtcmgr.StreamInfo{}
			for name := range p.live {
				prod := go2rtcmgr.ProducerInfo{URL: "wyze://test"}
				if p.media[name] {
					prod.Medias = []interface{}{"video, recvonly"}
				}
				result[name] = &go2rtcmgr.StreamInfo{Producers: []go2rtcmgr.ProducerInfo{prod}}
			}
			json.NewEncoder(w).Encode(result)
		case r.URL.Path == "/api/frame.mp4":
			if p.live[r.URL.Query().Get("src")] {
				w.Write([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}) // MP4 ftyp box
				return
			}
			w.WriteHeader(404)
		default:
			w.WriteHeader(200)
		}
	}))
	t.Cleanup(srv.Close)
	return p, go2rtcmgr.NewAPIClient(srv.URL, zerolog.Nop())
}

func (p *probeGo2RTC) set(name string, live, media bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.live[name] = live
	p.media[name] = media
}

func probeManager(t *testing.T) (*Manager, *probeGo2RTC) {
	t.Helper()
	fake, api := newProbeGo2RTC(t)
	cfg := &config.Config{Quality: "hd", Audio: true, CamOverrides: map[string]config.CamOverride{}, RefreshInterval: 30 * time.Minute}
	mgr := NewManager(cfg, wyzeapi.NewClient(wyzeapi.Credentials{}, "test", zerolog.Nop()), api, zerolog.Nop())
	mgr.SetHealthProbe(true)
	return mgr, fake
}

// streamingCam injects a camera already in StateStreaming.
func streamingCam(mgr *Manager, name string) *Camera {
	cam := NewCamera(wyzeapi.CameraInfo{Name: name, Model: "HL_CAM4"}, "hd", true, false)
	cam.SetState(StateStreaming)
	mgr.InjectCamera(name, cam)
	return cam
}

func TestHealthCheck_probeDemotesDeadCamera(t *testing.T) {
	mgr, fake := probeManager(t)
	ctx := context.Background()

	alive := streamingCam(mgr, "alive")
	dead := streamingCam(mgr, "dead")
	fake.set("alive", true, false) // reachable, but idle (no media) -> active probe succeeds
	fake.set("dead", false, false) // unreachable -> probe 404

	mgr.HealthCheck(ctx)

	if got := alive.GetState(); got != StateStreaming {
		t.Errorf("alive state = %v, want streaming", got)
	}
	if got := dead.GetState(); got != StateError {
		t.Errorf("dead state = %v, want error", got)
	}
	if dead.GetErrorCount() == 0 {
		t.Error("dead camera error count should have incremented (backoff)")
	}
}

// A reachable camera must reach Streaming only after the connect-time probe
// confirms media — proving we no longer report optimistic "streaming".
func TestConnectCamera_probeGatesStreaming(t *testing.T) {
	mgr, fake := probeManager(t)
	cam := NewCamera(wyzeapi.CameraInfo{
		Name: "alive", LanIP: "10.0.0.1", P2PID: "UID0", ENR: "e", MAC: "AABB01", Model: "HL_CAM4", DTLS: true,
	}, "hd", true, false)
	mgr.InjectCamera("alive", cam)
	fake.set("alive", true, false)

	mgr.connectCamera(context.Background(), cam)

	if got := cam.GetState(); got != StateStreaming {
		t.Errorf("state = %v, want streaming after successful probe", got)
	}
}

// A persistently dead camera must never flap to Streaming through the connect
// path, and each failed attempt must grow the backoff (error count).
func TestConnectCamera_probeNeverGreenWhenDead(t *testing.T) {
	mgr, fake := probeManager(t)
	ctx := context.Background()
	cam := NewCamera(wyzeapi.CameraInfo{
		Name: "dead", LanIP: "10.0.0.9", P2PID: "UID9", ENR: "e", MAC: "AABB09", Model: "HL_CAM4", DTLS: true,
	}, "hd", true, false)
	mgr.InjectCamera("dead", cam)
	fake.set("dead", false, false) // AddStream succeeds, but frame.mp4 404s

	mgr.connectCamera(ctx, cam)
	if got := cam.GetState(); got != StateError {
		t.Fatalf("attempt 1 state = %v, want error", got)
	}
	first := cam.GetErrorCount()

	mgr.connectCamera(ctx, cam)
	if got := cam.GetState(); got != StateError {
		t.Fatalf("attempt 2 state = %v, want error (no flap to streaming)", got)
	}
	if cam.GetErrorCount() <= first {
		t.Errorf("error count = %d, want > %d (backoff growth)", cam.GetErrorCount(), first)
	}
}

func TestHealthCheck_passiveFastPathSkipsProbe(t *testing.T) {
	mgr, fake := probeManager(t)
	ctx := context.Background()

	// Stream has a media-carrying producer (actively consumed). Even though
	// frame.mp4 would 404, the passive fast-path must keep it Streaming
	// without ever probing.
	cam := streamingCam(mgr, "consumed")
	fake.set("consumed", false /*frame would 404*/, true /*producer has media*/)

	mgr.HealthCheck(ctx)

	if got := cam.GetState(); got != StateStreaming {
		t.Errorf("consumed state = %v, want streaming (passive fast-path)", got)
	}
	if cam.GetErrorCount() != 0 {
		t.Errorf("consumed error count = %d, want 0 (never probed)", cam.GetErrorCount())
	}
}

func TestHealthCheck_legacyPassiveWhenProbeOff(t *testing.T) {
	fake, api := newProbeGo2RTC(t)
	cfg := &config.Config{Quality: "hd", CamOverrides: map[string]config.CamOverride{}, RefreshInterval: 30 * time.Minute}
	mgr := NewManager(cfg, wyzeapi.NewClient(wyzeapi.Credentials{}, "test", zerolog.Nop()), api, zerolog.Nop())
	// Probe OFF (default): legacy behavior — a registered producer (even with
	// no media, frame 404) is treated as healthy and left Streaming.
	cam := streamingCam(mgr, "legacy")
	fake.set("legacy", false, false) // producer present (registered), no media

	mgr.HealthCheck(context.Background())

	if got := cam.GetState(); got != StateStreaming {
		t.Errorf("legacy state = %v, want streaming (passive producer-count, no probe)", got)
	}
}
