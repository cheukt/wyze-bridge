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
// given cameras — no real Wyze auth or go2rtc subprocess.
func newTestService(t *testing.T, rtspPort int, cams map[string]*camera.Camera) *service {
	t.Helper()
	mgr := camera.NewManager(&config.Config{}, nil, nil, zerolog.Nop())
	for name, cam := range cams {
		mgr.InjectCamera(name, cam)
	}
	return &service{camMgr: mgr, rtspPort: rtspPort}
}

func injectableCamera(name, nickname, model string) *camera.Camera {
	return camera.NewCamera(
		wyzeapi.CameraInfo{Name: name, Nickname: nickname, Model: model},
		"hd", true, false,
	)
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

func TestDoCommand_listCameras(t *testing.T) {
	s := newTestService(t, 8554, map[string]*camera.Camera{
		"front_door": injectableCamera("front_door", "Front Door", "WYZE_CAKP2JFUS"),
		"backyard":   injectableCamera("backyard", "Backyard", "HL_CAM4"),
	})

	out := listNoProbe(t, s)

	cams, ok := out["cameras"].([]interface{})
	if !ok {
		t.Fatalf("cameras = %T, want []interface{}", out["cameras"])
	}
	if len(cams) != 2 {
		t.Fatalf("len(cameras) = %d, want 2", len(cams))
	}

	// Cameras() sorts by name, so backyard comes first.
	first := cams[0].(map[string]interface{})
	if first["name"] != "backyard" {
		t.Errorf("cameras[0].name = %v, want backyard", first["name"])
	}
	if first["rtsp_url"] != "rtsp://127.0.0.1:8554/backyard" {
		t.Errorf("cameras[0].rtsp_url = %v, want rtsp://127.0.0.1:8554/backyard", first["rtsp_url"])
	}

	second := cams[1].(map[string]interface{})
	if second["name"] != "front_door" {
		t.Errorf("cameras[1].name = %v, want front_door", second["name"])
	}
	if second["nickname"] != "Front Door" {
		t.Errorf("cameras[1].nickname = %v, want Front Door", second["nickname"])
	}
	if second["model"] != "WYZE_CAKP2JFUS" {
		t.Errorf("cameras[1].model = %v, want WYZE_CAKP2JFUS", second["model"])
	}
	if second["state"] != "offline" {
		t.Errorf("cameras[1].state = %v, want offline", second["state"])
	}
}

func TestDoCommand_rtspPortReflected(t *testing.T) {
	s := newTestService(t, 9000, map[string]*camera.Camera{
		"cam": injectableCamera("cam", "Cam", "M"),
	})
	out := listNoProbe(t, s)
	cam := out["cameras"].([]interface{})[0].(map[string]interface{})
	if cam["rtsp_url"] != "rtsp://127.0.0.1:9000/cam" {
		t.Errorf("rtsp_url = %v, want port 9000", cam["rtsp_url"])
	}
}

// With probe enabled (the default) and no live go2rtc, every camera must come
// back not-ready: no rtsp_url, an error reason, and no false "streaming".
func TestDoCommand_listCameras_probeMarksUnready(t *testing.T) {
	cam := injectableCamera("cam", "Cam", "HL_CAM4")
	cam.SetState(camera.StateStreaming) // pretend the optimistic state lied
	s := newTestService(t, 8554, map[string]*camera.Camera{"cam": cam})

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

func TestConfig_Validate_rdkSignature(t *testing.T) {
	// The rdk-facing Validate returns (required, optional, err).
	req, opt, err := (&Config{CredsFile: "/x/creds.env"}).Validate("svc")
	if err != nil {
		t.Fatalf("Validate ok case error: %v", err)
	}
	if len(req) != 0 || len(opt) != 0 {
		t.Errorf("deps = (%v, %v), want none", req, opt)
	}
	if _, _, err := (&Config{}).Validate("svc"); err == nil {
		t.Error("Validate blank creds_file = nil error, want error")
	}
}

// list_cameras must carry the dialed address and Wyze's own reachability
// verdict — without them a "discovery timeout" is indistinguishable from the
// camera simply being off the network.
func TestDoCommand_listCameras_reportsIPAndOnline(t *testing.T) {
	offline := camera.NewCamera(
		wyzeapi.CameraInfo{Name: "litterbox", Nickname: "Litterbox", Model: "HL_CAM4",
			LanIP: "10.0.0.9", Online: false},
		"hd", true, false,
	)
	online := camera.NewCamera(
		wyzeapi.CameraInfo{Name: "porch", Nickname: "Porch", Model: "HL_CAM4",
			LanIP: "10.0.0.10", Online: true},
		"hd", true, false,
	)
	s := newTestService(t, 8554, map[string]*camera.Camera{
		"litterbox": offline,
		"porch":     online,
	})

	cams := listNoProbe(t, s)["cameras"].([]interface{})
	got := map[string]map[string]interface{}{}
	for _, c := range cams {
		e := c.(map[string]interface{})
		got[e["name"].(string)] = e
	}

	if got["litterbox"]["ip"] != "10.0.0.9" {
		t.Errorf("litterbox.ip = %v, want 10.0.0.9", got["litterbox"]["ip"])
	}
	if got["litterbox"]["online"] != false {
		t.Errorf("litterbox.online = %v, want false", got["litterbox"]["online"])
	}
	if got["porch"]["ip"] != "10.0.0.10" {
		t.Errorf("porch.ip = %v, want 10.0.0.10", got["porch"]["ip"])
	}
	if got["porch"]["online"] != true {
		t.Errorf("porch.online = %v, want true", got["porch"]["online"])
	}
}
