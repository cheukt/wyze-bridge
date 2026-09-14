package viammod

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// newTestService builds a service backed by a camera.Manager seeded with the
// given cameras — no real Wyze auth or go2rtc subprocess. api may be nil for
// the commands that never reach Wyze.
func newTestService(t *testing.T, rtspPort int, api eventLister, cams ...*camera.Camera) *service {
	t.Helper()
	mgr := camera.NewManager(&config.Config{}, nil, nil, zerolog.Nop())
	for _, cam := range cams {
		mgr.InjectCamera(cam.Name(), cam)
	}
	return &service{camMgr: mgr, api: api, rtspPort: rtspPort}
}

func testCamera(info wyzeapi.CameraInfo) *camera.Camera {
	return camera.NewCamera(info, "hd", true, false)
}

// listNoProbe lists cameras without the active go2rtc probe (the test manager
// has no go2rtc API attached). The probe path is covered separately.
func listNoProbe(t *testing.T, s *service) map[string]interface{} {
	t.Helper()
	out, err := s.DoCommand(context.Background(), map[string]interface{}{
		"list_cameras": map[string]interface{}{"probe": false},
	})
	if err != nil {
		t.Fatalf("DoCommand(list_cameras) error: %v", err)
	}
	return out
}

// One list_cameras call must carry everything a consumer needs: names sorted,
// a URL on the configured RTSP port, the shaped metadata, and Wyze's own
// address + reachability verdict — without those last two a "discovery
// timeout" is indistinguishable from the camera being off the network.
func TestDoCommand_listCameras(t *testing.T) {
	s := newTestService(t, 9000, nil,
		testCamera(wyzeapi.CameraInfo{Name: "front_door", Nickname: "Front Door",
			Model: "WYZE_CAKP2JFUS", LanIP: "10.0.0.9", Online: false}),
		testCamera(wyzeapi.CameraInfo{Name: "backyard", Nickname: "Backyard",
			Model: "HL_CAM4", LanIP: "10.0.0.10", Online: true}),
	)

	out := listNoProbe(t, s)
	cams, ok := out["cameras"].([]interface{})
	if !ok {
		t.Fatalf("cameras is not a list: %#v", out["cameras"])
	}
	if len(cams) != 2 {
		t.Fatalf("len(cameras) = %d, want 2", len(cams))
	}

	// Cameras() sorts by name, so backyard comes first.
	want := []map[string]interface{}{
		{"name": "backyard", "nickname": "Backyard", "model": "HL_CAM4", "state": "offline",
			"ip": "10.0.0.10", "online": true, "rtsp_url": "rtsp://127.0.0.1:9000/backyard"},
		{"name": "front_door", "nickname": "Front Door", "model": "WYZE_CAKP2JFUS", "state": "offline",
			"ip": "10.0.0.9", "online": false, "rtsp_url": "rtsp://127.0.0.1:9000/front_door"},
	}
	for i, w := range want {
		got, ok := cams[i].(map[string]interface{})
		if !ok {
			t.Fatalf("cameras[%d] is not a map: %#v", i, cams[i])
		}
		for k, v := range w {
			if got[k] != v {
				t.Errorf("cameras[%d].%s = %v, want %v", i, k, got[k], v)
			}
		}
	}
}

// With probe enabled (the default) and no live go2rtc, every camera must come
// back not-ready: no rtsp_url, an error reason, and no false "streaming".
func TestDoCommand_listCameras_probeMarksUnready(t *testing.T) {
	cam := testCamera(wyzeapi.CameraInfo{Name: "cam", Nickname: "Cam", Model: "HL_CAM4"})
	cam.SetState(camera.StateStreaming) // pretend the optimistic state lied
	s := newTestService(t, 8554, nil, cam)

	out, err := s.DoCommand(context.Background(), map[string]interface{}{"list_cameras": true})
	if err != nil {
		t.Fatalf("DoCommand error: %v", err)
	}
	got := out["cameras"].([]interface{})[0].(map[string]interface{})

	if got["ready"] != false {
		t.Errorf("ready = %v, want false", got["ready"])
	}
	if _, hasURL := got["rtsp_url"]; hasURL {
		t.Errorf("rtsp_url present for unready camera: %v", got["rtsp_url"])
	}
	if got["state"] != "error" {
		t.Errorf("state = %v, want error (downgraded from streaming)", got["state"])
	}
	if got["error"] == nil || got["error"] == "" {
		t.Errorf("error reason missing for unready camera")
	}
}

func TestDoCommand_errors(t *testing.T) {
	s := newTestService(t, 8554, nil)

	tests := []struct {
		name string
		cmd  map[string]interface{}
	}{
		{"unknown command", map[string]interface{}{"frobnicate": true}},
		{"restart_camera non-string", map[string]interface{}{"restart_camera": 42}},
		{"restart_camera empty", map[string]interface{}{"restart_camera": ""}},
		{"set_quality not object", map[string]interface{}{"set_quality": "hd"}},
		{"set_quality missing fields", map[string]interface{}{"set_quality": map[string]interface{}{"name": "cam"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.DoCommand(context.Background(), tt.cmd); err == nil {
				t.Errorf("DoCommand(%v) = nil error, want error", tt.cmd)
			}
		})
	}
}
