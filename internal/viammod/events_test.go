package viammod

import (
	"context"
	"errors"
	"testing"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// fakeEventLister is a canned eventLister so get_events needs no real Wyze auth.
type fakeEventLister struct {
	events  []map[string]interface{}
	err     error
	gotMACs []string
	gotMS   [2]int64
}

func (f *fakeEventLister) GetEventList(_ context.Context, macs []string, beginMS, endMS int64) ([]map[string]interface{}, error) {
	f.gotMACs = macs
	f.gotMS = [2]int64{beginMS, endMS}
	return f.events, f.err
}

func camWithMAC(name, nickname, mac string) *camera.Camera {
	return testCamera(wyzeapi.CameraInfo{Name: name, Nickname: nickname, MAC: mac})
}

func TestDoCommand_getEvents_shapesAndResolves(t *testing.T) {
	api := &fakeEventLister{events: []map[string]interface{}{
		{
			"device_id":    "AABBCCDDEEFF",
			"device_model": "HL_CAM4",
			"event_ts":     float64(1_700_000_000_000),
			"event_value":  "1",
			"event_id":     "evt-1",
			"tag_list":     []interface{}{"person", 7, "vehicle"},
			"file_list": []interface{}{
				map[string]interface{}{"type": float64(1), "url": "https://wyze/thumb.jpg"},
				map[string]interface{}{"type": float64(2), "url": "https://wyze/clip.mp4"},
			},
		},
	}}
	cam := camWithMAC("front_door", "Front Door", "AABBCCDDEEFF")
	s := newTestService(t, 8554, api, cam)

	out, err := s.DoCommand(context.Background(), map[string]interface{}{"get_events": true})
	if err != nil {
		t.Fatalf("DoCommand(get_events) error: %v", err)
	}

	if out["window_seconds"] != 300 {
		t.Errorf("window_seconds = %v, want 300", out["window_seconds"])
	}
	events := out["events"].([]interface{})
	if len(events) != 1 {
		t.Fatalf("len(events) = %d, want 1", len(events))
	}
	e := events[0].(map[string]interface{})

	want := map[string]interface{}{
		"camera": "front_door", "nickname": "Front Door", "model": "HL_CAM4",
		"event_id": "evt-1", "time": "2023-11-14T22:13:20Z",
		"thumbnail_url": "https://wyze/thumb.jpg", "video_url": "https://wyze/clip.mp4",
	}
	for k, v := range want {
		if e[k] != v {
			t.Errorf("%s = %v, want %v", k, e[k], v)
		}
	}
	// Non-string tag entries are dropped.
	if tags := e["tags"].([]string); len(tags) != 2 || tags[0] != "person" || tags[1] != "vehicle" {
		t.Errorf("tags = %v, want [person vehicle]", tags)
	}
	if _, hasRaw := e["file_list"]; hasRaw {
		t.Error("raw file_list leaked into shaped event")
	}
}

func TestGetEvents_resolvesCameraFromEventID(t *testing.T) {
	// The live response carries no device_mac; the MAC is the event_id prefix.
	api := &fakeEventLister{events: []map[string]interface{}{
		{"event_id": "80482CAA9F2F011782509992", "event_ts": float64(1782509992799), "event_value": "1"},
	}}
	s := newTestService(t, 8554, api, camWithMAC("garage", "Garage", "80482CAA9F2F"))

	out, err := s.getEvents(context.Background(), defaultEventWindow)
	if err != nil {
		t.Fatalf("getEvents error: %v", err)
	}
	e := out["events"].([]interface{})[0].(map[string]interface{})
	if e["mac"] != "80482CAA9F2F" {
		t.Errorf("mac = %v, want 80482CAA9F2F (from event_id prefix)", e["mac"])
	}
	if e["camera"] != "garage" {
		t.Errorf("camera = %v, want garage", e["camera"])
	}
}

func TestGetEvents_passesAllCameraMACs(t *testing.T) {
	api := &fakeEventLister{}
	s := newTestService(t, 8554, api,
		camWithMAC("a", "A", "MAC-A"),
		camWithMAC("b", "B", "MAC-B"),
		camWithMAC("noMAC", "NoMAC", ""), // skipped — no MAC
	)

	if _, err := s.getEvents(context.Background(), defaultEventWindow); err != nil {
		t.Fatalf("getEvents error: %v", err)
	}
	if len(api.gotMACs) != 2 {
		t.Fatalf("got %d MACs, want 2: %v", len(api.gotMACs), api.gotMACs)
	}
	if api.gotMS[1] <= api.gotMS[0] {
		t.Errorf("end %d not after begin %d", api.gotMS[1], api.gotMS[0])
	}
	if got := api.gotMS[1] - api.gotMS[0]; got != defaultEventWindow.Milliseconds() {
		t.Errorf("window span = %dms, want %dms", got, defaultEventWindow.Milliseconds())
	}
}

func TestGetEvents_returnsAllEvents(t *testing.T) {
	api := &fakeEventLister{events: []map[string]interface{}{
		{"device_id": "M", "event_value": "1"},
		{"device_id": "M", "event_value": "2"},
		{"device_id": "M"},
	}}
	s := newTestService(t, 8554, api, camWithMAC("cam", "Cam", "M"))

	out, err := s.getEvents(context.Background(), defaultEventWindow)
	if err != nil {
		t.Fatalf("getEvents error: %v", err)
	}
	if got := len(out["events"].([]interface{})); got != 3 {
		t.Errorf("len(events) = %d, want 3 (no filtering)", got)
	}
}

func TestGetEvents_noCamerasSkipsAPI(t *testing.T) {
	api := &fakeEventLister{events: []map[string]interface{}{{"device_mac": "X"}}}
	s := newTestService(t, 8554, api) // no cameras

	out, err := s.getEvents(context.Background(), defaultEventWindow)
	if err != nil {
		t.Fatalf("getEvents error: %v", err)
	}
	if api.gotMACs != nil {
		t.Errorf("API called with %v, want no call when no cameras", api.gotMACs)
	}
	if got := len(out["events"].([]interface{})); got != 0 {
		t.Errorf("len(events) = %d, want 0", got)
	}
}

func TestGetEvents_windowOverride(t *testing.T) {
	api := &fakeEventLister{}
	s := newTestService(t, 8554, api, camWithMAC("cam", "Cam", "M"))

	out, err := s.DoCommand(context.Background(), map[string]interface{}{
		"get_events": map[string]interface{}{"window_seconds": float64(120)},
	})
	if err != nil {
		t.Fatalf("DoCommand error: %v", err)
	}
	if out["window_seconds"] != 120 {
		t.Errorf("window_seconds = %v, want 120", out["window_seconds"])
	}
	if got := api.gotMS[1] - api.gotMS[0]; got != 120_000 {
		t.Errorf("window span = %dms, want 120000ms", got)
	}
}

func TestGetEvents_apiError(t *testing.T) {
	api := &fakeEventLister{err: errors.New("boom")}
	s := newTestService(t, 8554, api, camWithMAC("cam", "Cam", "M"))

	if _, err := s.getEvents(context.Background(), defaultEventWindow); err == nil {
		t.Error("getEvents = nil error, want propagated API error")
	}
}
