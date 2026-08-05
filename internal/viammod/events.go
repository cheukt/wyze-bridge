package viammod

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
)

// defaultEventWindow is how far back get_events looks when the caller doesn't
// specify a window.
const defaultEventWindow = 5 * time.Minute

// eventLister is the slice of the Wyze API the events surface needs. *wyzeapi.Client
// satisfies it; tests substitute a fake so get_events needs no real auth.
type eventLister interface {
	GetEventList(ctx context.Context, macs []string, beginTimeMS, endTimeMS int64) ([]map[string]interface{}, error)
}

// getEvents fetches recent events across all known cameras within window and
// returns them shaped, newest the API gives first. Resolves each event's
// device_id (the camera MAC) back to the friendly name.
func (s *service) getEvents(ctx context.Context, window time.Duration) (map[string]interface{}, error) {
	cams := s.camMgr.Cameras()
	macs := make([]string, 0, len(cams))
	byMAC := make(map[string]*camera.Camera, len(cams))
	for _, cam := range cams {
		mac := cam.GetInfo().MAC
		if mac == "" {
			continue
		}
		macs = append(macs, mac)
		byMAC[mac] = cam
	}

	events := make([]interface{}, 0)
	if len(macs) > 0 {
		end := time.Now()
		begin := end.Add(-window)
		raw, err := s.api.GetEventList(ctx, macs, begin.UnixMilli(), end.UnixMilli())
		if err != nil {
			return nil, fmt.Errorf("get_events: %w", err)
		}
		for _, e := range raw {
			// Dump the raw event shape for troubleshooting (signed thumbnail
			// URLs, AI tag_list contents, etc. that the metadata view drops).
			s.log.Debug().Interface("event", e).Msg("raw wyze event")
			events = append(events, shapeEvent(e, byMAC))
		}
	}

	return map[string]interface{}{
		"events":         events,
		"window_seconds": int(window.Seconds()),
	}, nil
}

// shapeEvent reduces a raw Wyze event to a clean map. Only fields that are
// present are emitted, including the signed media URLs from file_list
// (thumbnail_url for the image, video_url for the clip).
func shapeEvent(e map[string]interface{}, byMAC map[string]*camera.Camera) map[string]interface{} {
	mac := eventMAC(e)
	out := map[string]interface{}{"mac": mac}

	if cam := byMAC[mac]; cam != nil {
		out["camera"] = cam.Name()
		out["nickname"] = cam.GetInfo().Nickname
	}
	if m, ok := e["device_model"].(string); ok && m != "" {
		out["model"] = m
	}
	if ts, ok := asInt(e["event_ts"]); ok && ts > 0 {
		out["event_ts"] = ts
		out["time"] = time.UnixMilli(ts).UTC().Format(time.RFC3339)
	}
	if v, ok := e["event_value"]; ok {
		out["value"] = v
	}
	if id, ok := e["event_id"].(string); ok && id != "" {
		out["event_id"] = id
	}
	if tags := stringList(e["tag_list"]); len(tags) > 0 {
		out["tags"] = tags
	}
	if thumb, video := fileURLs(e["file_list"]); thumb != "" || video != "" {
		if thumb != "" {
			out["thumbnail_url"] = thumb
		}
		if video != "" {
			out["video_url"] = video
		}
	}
	return out
}

// fileURLs pulls the first image and video URL out of an event's file_list.
// Wyze tags each entry with a type: 1 = image (thumbnail), 2 = video clip.
func fileURLs(v interface{}) (thumbnail, video string) {
	raw, ok := v.([]interface{})
	if !ok {
		return "", ""
	}
	for _, item := range raw {
		f, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		url, _ := f["url"].(string)
		if url == "" {
			continue
		}
		switch t, _ := asInt(f["type"]); t {
		case 1:
			if thumbnail == "" {
				thumbnail = url
			}
		case 2:
			if video == "" {
				video = url
			}
		}
	}
	return thumbnail, video
}

// eventMAC extracts the camera MAC from a raw Wyze event, uppercased to match
// CameraInfo.MAC (which is stored uppercase, no colons). It prefers an explicit
// device field but falls back to the event_id prefix: Wyze event_ids are
// "<MAC><timestamp...>", and the MAC is the leading 12 hex chars — the only
// camera identifier this response actually carries.
func eventMAC(e map[string]interface{}) string {
	for _, k := range []string{"device_mac", "device_id"} {
		if s, ok := e[k].(string); ok && s != "" {
			return strings.ToUpper(s)
		}
	}
	if id, ok := e["event_id"].(string); ok && len(id) >= 12 {
		if prefix := strings.ToUpper(id[:12]); isHex(prefix) {
			return prefix
		}
	}
	return ""
}

// isHex reports whether s is all hexadecimal digits.
func isHex(s string) bool {
	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		isLower := r >= 'a' && r <= 'f'
		isUpper := r >= 'A' && r <= 'F'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return s != ""
}

// asInt coerces a JSON-decoded number (float64) or numeric string into an int64.
func asInt(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i, err == nil
	default:
		return 0, false
	}
}

// stringList extracts a []interface{} of strings (e.g. AI detection tags) into a
// plain []string, skipping non-string entries.
func stringList(v interface{}) []string {
	raw, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// eventWindow resolves the lookback window from the get_events argument.
// Accepts a bare truthy value (defaults to defaultEventWindow) or an object
// {"get_events": {"window_seconds": 120}} to widen/narrow it.
func eventWindow(v interface{}) time.Duration {
	if obj, ok := v.(map[string]interface{}); ok {
		if secs, ok := obj["window_seconds"].(float64); ok && secs > 0 {
			return time.Duration(secs) * time.Second
		}
	}
	return defaultEventWindow
}
