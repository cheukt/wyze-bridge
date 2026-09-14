package viammod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// newDiscoveryTestManager wires a Manager to a Wyze API stub served by handler.
func newDiscoveryTestManager(t *testing.T, handler http.HandlerFunc) *camera.Manager {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	api := wyzeapi.NewClient(wyzeapi.Credentials{}, "test", zerolog.Nop())
	api.WyzeURL = srv.URL
	// Skip the login round trip; only discovery is under test.
	api.SetAuth(&wyzeapi.AuthState{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour)})

	cfg := &config.Config{Quality: "hd", CamOverrides: map[string]config.CamOverride{}}
	return camera.NewManager(cfg, api, nil, zerolog.Nop())
}

// rateLimitedThen answers with an empty-bodied 429 — the failure that stranded
// the module — for the first n calls, then serves devices. The counter tracks
// every call, failed or not.
func rateLimitedThen(n int32, devices []interface{}) (http.HandlerFunc, *atomic.Int32) {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= n {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": "1",
			"data": map[string]interface{}{"device_list": devices},
		})
	}, &calls
}

func oneCamera() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"product_type":  "Camera",
			"product_model": "WYZE_CAKP2JFUS",
			"mac":           "AABB01",
			"nickname":      "Front Door",
			"enr":           "enr1",
			"device_params": map[string]interface{}{"ip": "10.0.0.1", "p2p_id": "UID01234567890123456"},
		},
	}
}

func TestAwaitInitialDiscoveryRetriesUntilSuccess(t *testing.T) {
	handler, calls := rateLimitedThen(2, oneCamera())
	mgr := newDiscoveryTestManager(t, handler)

	if !awaitInitialDiscovery(context.Background(), mgr, time.Millisecond, zerolog.Nop()) {
		t.Fatal("want success after the 429s clear")
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("want 3 attempts (2 failed + 1 succeeded), got %d", got)
	}
	if got := len(mgr.Cameras()); got != 1 {
		t.Fatalf("want 1 camera registered, got %d", got)
	}
}

// A successful discovery that finds no cameras still hands off — otherwise an
// empty account or a non-matching filter would block RunDiscoveryLoop forever.
func TestAwaitInitialDiscoveryReturnsOnEmptyResult(t *testing.T) {
	handler, _ := rateLimitedThen(0, []interface{}{})
	mgr := newDiscoveryTestManager(t, handler)

	if !awaitInitialDiscovery(context.Background(), mgr, time.Millisecond, zerolog.Nop()) {
		t.Fatal("want success even with zero cameras discovered")
	}
}

func TestAwaitInitialDiscoveryStopsOnContextCancel(t *testing.T) {
	handler, calls := rateLimitedThen(1<<30, nil) // never succeeds
	mgr := newDiscoveryTestManager(t, handler)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() { done <- awaitInitialDiscovery(ctx, mgr, time.Hour, zerolog.Nop()) }()

	// Let the first attempt land, so cancellation is observed at the wait.
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("want false on cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitInitialDiscovery ignored context cancellation")
	}
}
