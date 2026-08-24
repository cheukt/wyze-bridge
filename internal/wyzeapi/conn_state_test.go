package wyzeapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

// Wyze keeps serving a camera's last-known LanIP after it drops off the
// network; conn_state is what distinguishes a dialable address from a stale
// one. An absent conn_state must read as online so a response-shape change
// can't mark the whole fleet offline.
func TestConnStateOnline(t *testing.T) {
	tests := []struct {
		name string
		dev  map[string]interface{}
		want bool
	}{
		{"absent", map[string]interface{}{}, true},
		{"online float", map[string]interface{}{"conn_state": float64(1)}, true},
		{"offline float", map[string]interface{}{"conn_state": float64(0)}, false},
		{"online int", map[string]interface{}{"conn_state": 1}, true},
		{"offline int", map[string]interface{}{"conn_state": 0}, false},
		{"online string", map[string]interface{}{"conn_state": "1"}, true},
		{"offline string", map[string]interface{}{"conn_state": "0"}, false},
		{"empty string", map[string]interface{}{"conn_state": ""}, false},
		{"unexpected type", map[string]interface{}{"conn_state": true}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connStateOnline(tt.dev); got != tt.want {
				t.Errorf("connStateOnline(%v) = %v, want %v", tt.dev, got, tt.want)
			}
		})
	}
}

// A camera Wyze reports as offline still carries a stale LanIP, so it survives
// the missing-P2P-params check and reaches the manager — which is exactly the
// case where Online is the only signal that the address is not dialable.
func TestClient_GetCameraList_offlineCameraKeepsStaleIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user/login" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code": "1",
				"data": map[string]interface{}{
					"access_token": "tok", "refresh_token": "ref", "user_id": "uid",
				},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": "1",
			"data": map[string]interface{}{
				"device_list": []interface{}{
					map[string]interface{}{
						"product_type": "Camera", "product_model": "HL_CAM4",
						"mac": "AABB01", "nickname": "Litterbox", "enr": "e1",
						"conn_state": float64(0),
						"device_params": map[string]interface{}{
							"p2p_id": "UID01234567890123456", "ip": "10.0.0.9",
						},
					},
					map[string]interface{}{
						"product_type": "Camera", "product_model": "HL_CAM4",
						"mac": "AABB02", "nickname": "Porch", "enr": "e2",
						"conn_state": float64(1),
						"device_params": map[string]interface{}{
							"p2p_id": "UID01234567890123457", "ip": "10.0.0.10",
						},
					},
				},
			},
		})
	}))
	defer server.Close()

	c := NewClient(Credentials{Email: "a@b.com", Password: "p", APIID: "i", APIKey: "k"}, "v", zerolog.Nop())
	c.AuthURL = server.URL
	c.WyzeURL = server.URL

	cams, err := c.GetCameraList()
	if err != nil {
		t.Fatalf("GetCameraList: %v", err)
	}
	if len(cams) != 2 {
		t.Fatalf("len(cams) = %d, want 2", len(cams))
	}

	byName := map[string]CameraInfo{}
	for _, cam := range cams {
		byName[cam.Nickname] = cam
	}
	if byName["Litterbox"].Online {
		t.Error("Litterbox: Online = true, want false (conn_state 0)")
	}
	if got := byName["Litterbox"].LanIP; got != "10.0.0.9" {
		t.Errorf("Litterbox: LanIP = %q, want the stale 10.0.0.9", got)
	}
	if !byName["Porch"].Online {
		t.Error("Porch: Online = false, want true (conn_state 1)")
	}
}
