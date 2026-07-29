package go2rtcmgr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"
)

func TestAPIClient_ListStreams(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/streams" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method != "GET" {
			t.Errorf("method = %q", r.Method)
		}
		json.NewEncoder(w).Encode(map[string]*StreamInfo{
			"front_door": {
				Producers: []ProducerInfo{{URL: "wyze://1.2.3.4"}},
			},
			"backyard": {},
		})
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	streams, err := c.ListStreams(context.Background())
	if err != nil {
		t.Fatalf("ListStreams: %v", err)
	}
	if len(streams) != 2 {
		t.Errorf("streams = %d, want 2", len(streams))
	}
	if len(streams["front_door"].Producers) != 1 {
		t.Error("front_door should have 1 producer")
	}
}

func TestAPIClient_AddStream(t *testing.T) {
	var gotName, gotSrc string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("method = %q, want PUT", r.Method)
		}
		gotName = r.URL.Query().Get("name")
		gotSrc = r.URL.Query().Get("src")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	err := c.AddStream(context.Background(), "test_cam", "wyze://1.2.3.4?uid=X")
	if err != nil {
		t.Fatalf("AddStream: %v", err)
	}
	if gotName != "test_cam" {
		t.Errorf("name = %q", gotName)
	}
	if gotSrc != "wyze://1.2.3.4?uid=X" {
		t.Errorf("src = %q", gotSrc)
	}
}

func TestAPIClient_DeleteStream(t *testing.T) {
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	err := c.DeleteStream(context.Background(), "test_cam")
	if err != nil {
		t.Fatalf("DeleteStream: %v", err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
}

func TestAPIClient_HasActiveProducer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]*StreamInfo{
			"active_cam": {
				Producers: []ProducerInfo{{URL: "wyze://1.2.3.4"}},
			},
			"idle_cam": {},
		})
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())

	active, err := c.HasActiveProducer(context.Background(), "active_cam")
	if err != nil {
		t.Fatalf("HasActiveProducer: %v", err)
	}
	if !active {
		t.Error("active_cam should have active producer")
	}

	idle, err := c.HasActiveProducer(context.Background(), "idle_cam")
	if err != nil {
		t.Fatal(err)
	}
	if idle {
		t.Error("idle_cam should not have active producer")
	}

	missing, err := c.HasActiveProducer(context.Background(), "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if missing {
		t.Error("nonexistent should not have active producer")
	}
}

func TestAPIClient_GetSnapshot(t *testing.T) {
	jpegData := []byte{0xFF, 0xD8, 0xFF, 0xE0} // JPEG magic bytes
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("src") != "test_cam" {
			t.Errorf("src = %q", r.URL.Query().Get("src"))
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpegData)
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	data, err := c.GetSnapshot(context.Background(), "test_cam")
	if err != nil {
		t.Fatalf("GetSnapshot: %v", err)
	}
	if len(data) != len(jpegData) {
		t.Errorf("data length = %d, want %d", len(data), len(jpegData))
	}
}

// go2rtc returns 200 OK with an empty body when the lazy source is registered
// but produces no frame (camera not actually streaming). GetSnapshot must treat
// that as an error, not a successful snapshot, so liveness probes stay honest.
func TestAPIClient_GetSnapshot_emptyBodyIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // Content-Length: 0, no body
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	if _, err := c.GetSnapshot(context.Background(), "dead_cam"); err == nil {
		t.Fatal("GetSnapshot = nil error for empty 200 body, want error")
	}
}

// A 200 with a non-JPEG payload (e.g. an HTML error page) is not a frame.
func TestAPIClient_GetSnapshot_nonJPEGIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>nope</html>"))
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	if _, err := c.GetSnapshot(context.Background(), "dead_cam"); err == nil {
		t.Fatal("GetSnapshot = nil error for non-JPEG body, want error")
	}
}

// ProbeFrame hits /api/frame.mp4 (codec copy, no ffmpeg) and accepts a body
// that opens with an MP4 'ftyp' box as proof of a live frame.
func TestAPIClient_ProbeFrame(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/frame.mp4" {
			t.Errorf("path = %q, want /api/frame.mp4", r.URL.Path)
		}
		if r.URL.Query().Get("src") != "test_cam" {
			t.Errorf("src = %q", r.URL.Query().Get("src"))
		}
		w.Write([]byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	if err := c.ProbeFrame(context.Background(), "test_cam"); err != nil {
		t.Fatalf("ProbeFrame: %v", err)
	}
}

// go2rtc answers 200 with an empty body when the lazy source is registered but
// never produces a frame. ProbeFrame must treat that as an error so liveness
// probes stay honest.
func TestAPIClient_ProbeFrame_emptyBodyIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // Content-Length: 0, no body
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	if err := c.ProbeFrame(context.Background(), "dead_cam"); err == nil {
		t.Fatal("ProbeFrame = nil error for empty 200 body, want error")
	}
}

// A 200 with a non-MP4 payload (e.g. an HTML error page) is not a frame.
func TestAPIClient_ProbeFrame_nonMP4IsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>nope</html>"))
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())
	if err := c.ProbeFrame(context.Background(), "dead_cam"); err == nil {
		t.Fatal("ProbeFrame = nil error for non-MP4 body, want error")
	}
}

func TestAPIClient_ErrorHandling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	c := NewAPIClient(server.URL, zerolog.Nop())

	_, err := c.ListStreams(context.Background())
	if err == nil {
		t.Error("expected error for 500 response")
	}

	err = c.AddStream(context.Background(), "x", "y")
	if err == nil {
		t.Error("expected error for 500 response")
	}
}

func TestAPIClient_ConnectionRefused(t *testing.T) {
	c := NewAPIClient("http://localhost:19999", zerolog.Nop())

	_, err := c.ListStreams(context.Background())
	if err == nil {
		t.Error("expected error for connection refused")
	}
}
