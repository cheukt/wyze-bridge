package viammod

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.viam.com/rdk/components/camera"
	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/pointcloud"
	"go.viam.com/rdk/resource"
	"go.viam.com/rdk/services/generic"
	"go.viam.com/rdk/spatialmath"
)

// fakeManager is a canned generic-service resource whose DoCommand returns a
// scripted response and counts calls, so the poll logic needs no real manager.
type fakeManager struct {
	resource.Named
	resource.TriviallyReconfigurable
	resource.TriviallyCloseable

	mu      sync.Mutex
	calls   int
	lastCmd map[string]interface{}
	resp    map[string]interface{}
	err     error
}

func (f *fakeManager) DoCommand(_ context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	f.mu.Lock()
	f.calls++
	f.lastCmd = cmd
	f.mu.Unlock()
	return f.resp, f.err
}

func (f *fakeManager) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeManager) lastCommand() map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastCmd
}

func newFakeManager(resp map[string]interface{}, err error) *fakeManager {
	return &fakeManager{Named: generic.Named("mgr").AsNamed(), resp: resp, err: err}
}

// fakeCam is a minimal camera.Camera whose Images returns a canned frame.
type fakeCam struct {
	resource.Named
	resource.TriviallyReconfigurable
	resource.TriviallyCloseable

	imgs []camera.NamedImage
	meta resource.ResponseMetadata
	err  error
}

func (f *fakeCam) Images(_ context.Context, _ []string, _ map[string]interface{}) ([]camera.NamedImage, resource.ResponseMetadata, error) {
	return f.imgs, f.meta, f.err
}

func (f *fakeCam) NextPointCloud(context.Context, map[string]interface{}) (pointcloud.PointCloud, error) {
	return nil, errUnimplemented
}

func (f *fakeCam) Properties(context.Context) (camera.Properties, error) {
	return camera.Properties{}, nil
}

func (f *fakeCam) Geometries(context.Context, map[string]interface{}) ([]spatialmath.Geometry, error) {
	return nil, nil
}

// newTestCam builds a conditionalCamera wired to fakes with the given timings.
func newTestCam(t *testing.T, mgr resource.Resource, cam camera.Camera, window, cooldown time.Duration, camName string) *conditionalCamera {
	t.Helper()
	return &conditionalCamera{
		Named:    camera.Named("cond").AsNamed(),
		cam:      cam,
		manager:  mgr,
		logger:   logging.NewTestLogger(t),
		camName:  camName,
		window:   window,
		cooldown: cooldown,
		poll:     time.Second,
	}
}

// oneImageCam returns a fakeCam that yields a single (empty) named image so the
// Images passthrough has something to return.
func oneImageCam() *fakeCam {
	return &fakeCam{imgs: []camera.NamedImage{{}}}
}

func TestConditional_Images_gatesDataMgmt(t *testing.T) {
	cam := oneImageCam()
	cc := newTestCam(t, newFakeManager(nil, nil), cam, 20*time.Second, 5*time.Minute, "")

	dm := data.FromDMExtraMap // {"fromDataManagement": true}

	t.Run("active event, from data mgmt -> passes", func(t *testing.T) {
		cc.lastEventTS = time.Now()
		imgs, _, err := cc.Images(context.Background(), nil, dm)
		if err != nil {
			t.Fatalf("want images, got err %v", err)
		}
		if len(imgs) != 1 {
			t.Fatalf("want 1 image, got %d", len(imgs))
		}
	})

	t.Run("stale event, from data mgmt -> filtered", func(t *testing.T) {
		cc.lastEventTS = time.Now().Add(-30 * time.Second)
		_, _, err := cc.Images(context.Background(), nil, dm)
		if !errors.Is(err, data.ErrNoCaptureToStore) {
			t.Fatalf("want ErrNoCaptureToStore, got %v", err)
		}
	})

	t.Run("no event ever, from data mgmt -> filtered", func(t *testing.T) {
		cc.lastEventTS = time.Time{}
		_, _, err := cc.Images(context.Background(), nil, dm)
		if !errors.Is(err, data.ErrNoCaptureToStore) {
			t.Fatalf("want ErrNoCaptureToStore, got %v", err)
		}
	})

	t.Run("stale event, live view -> passes anyway", func(t *testing.T) {
		cc.lastEventTS = time.Now().Add(-30 * time.Second)
		imgs, _, err := cc.Images(context.Background(), nil, nil)
		if err != nil {
			t.Fatalf("live view should never be gated, got err %v", err)
		}
		if len(imgs) != 1 {
			t.Fatalf("want 1 image, got %d", len(imgs))
		}
	})
}

func TestConditional_Images_stampsEventID(t *testing.T) {
	dm := data.FromDMExtraMap

	// stampedCam builds a data-mgmt-gated cam with stamping enabled and an
	// active event carrying the given id and label prefix.
	stampedCam := func(t *testing.T, id, prefix string) *conditionalCamera {
		cc := newTestCam(t, newFakeManager(nil, nil), oneImageCam(), 20*time.Second, 5*time.Minute, "")
		cc.lastEventTS = time.Now()
		cc.lastEventID = id
		cc.stampEnabled = true
		cc.stampPrefix = prefix
		return cc
	}

	t.Run("data mgmt capture carries event id classification + bbox", func(t *testing.T) {
		cc := stampedCam(t, "abc123", defaultEventClassPrefix)
		imgs, _, err := cc.Images(context.Background(), nil, dm)
		if err != nil {
			t.Fatalf("want images, got err %v", err)
		}
		want := defaultEventClassPrefix + "abc123"
		classes := imgs[0].Annotations.Classifications
		if len(classes) != 1 || classes[0].Label != want {
			t.Fatalf("want a single %q classification, got %+v", want, classes)
		}
		boxes := imgs[0].Annotations.BoundingBoxes
		if len(boxes) != 1 || boxes[0].Label != want {
			t.Fatalf("want a single %q bounding box, got %+v", want, boxes)
		}
		if b := boxes[0]; b.XMinNormalized != 0 || b.YMinNormalized != 0 || b.XMaxNormalized != 1 || b.YMaxNormalized != 1 {
			t.Fatalf("want full-frame bbox [0,0,1,1], got %+v", b)
		}
	})

	t.Run("custom prefix is used", func(t *testing.T) {
		cc := stampedCam(t, "abc123", "cat_event:")
		imgs, _, err := cc.Images(context.Background(), nil, dm)
		if err != nil {
			t.Fatalf("want images, got err %v", err)
		}
		want := "cat_event:abc123"
		if classes := imgs[0].Annotations.Classifications; len(classes) != 1 || classes[0].Label != want {
			t.Fatalf("want a single %q classification, got %+v", want, classes)
		}
	})

	t.Run("stamping disabled -> no annotations even with active event", func(t *testing.T) {
		cc := newTestCam(t, newFakeManager(nil, nil), oneImageCam(), 20*time.Second, 5*time.Minute, "")
		cc.lastEventTS = time.Now()
		cc.lastEventID = "abc123"
		// stampEnabled defaults to false.

		imgs, _, err := cc.Images(context.Background(), nil, dm)
		if err != nil {
			t.Fatalf("want images, got err %v", err)
		}
		if a := imgs[0].Annotations; len(a.Classifications) != 0 || len(a.BoundingBoxes) != 0 {
			t.Fatalf("stamping disabled should add nothing, got %+v", a)
		}
	})

	t.Run("live view is not stamped", func(t *testing.T) {
		cc := stampedCam(t, "abc123", defaultEventClassPrefix)
		imgs, _, err := cc.Images(context.Background(), nil, nil)
		if err != nil {
			t.Fatalf("live view should pass, got err %v", err)
		}
		if len(imgs[0].Annotations.Classifications) != 0 {
			t.Fatalf("live view frames should not be stamped, got %+v", imgs[0].Annotations.Classifications)
		}
	})

	t.Run("empty event id -> no classification added", func(t *testing.T) {
		cc := stampedCam(t, "", defaultEventClassPrefix)
		imgs, _, err := cc.Images(context.Background(), nil, dm)
		if err != nil {
			t.Fatalf("want images, got err %v", err)
		}
		if a := imgs[0].Annotations; len(a.Classifications) != 0 || len(a.BoundingBoxes) != 0 {
			t.Fatalf("no id means no annotations, got %+v", a)
		}
	})
}

func TestConditional_resolveStamp(t *testing.T) {
	t.Run("nil -> disabled", func(t *testing.T) {
		if en, _ := resolveStamp(nil); en {
			t.Fatal("nil config should disable stamping")
		}
	})
	t.Run("enabled=false -> disabled", func(t *testing.T) {
		if en, _ := resolveStamp(&StampConfig{Enabled: false, LabelPrefix: "x:"}); en {
			t.Fatal("Enabled=false should disable stamping")
		}
	})
	t.Run("enabled, no prefix -> default prefix", func(t *testing.T) {
		en, p := resolveStamp(&StampConfig{Enabled: true})
		if !en || p != defaultEventClassPrefix {
			t.Fatalf("want enabled with default prefix %q, got en=%v p=%q", defaultEventClassPrefix, en, p)
		}
	})
	t.Run("enabled, custom prefix trimmed", func(t *testing.T) {
		en, p := resolveStamp(&StampConfig{Enabled: true, LabelPrefix: "  cat_event:  "})
		if !en || p != "cat_event:" {
			t.Fatalf("want enabled with trimmed custom prefix, got en=%v p=%q", en, p)
		}
	})
}

func TestConditional_maybePoll_skipsWithinCooldown(t *testing.T) {
	mgr := newFakeManager(map[string]interface{}{"events": []interface{}{}}, nil)
	cc := newTestCam(t, mgr, oneImageCam(), 20*time.Second, 5*time.Minute, "")

	// Event 10s ago: inside the 20s window AND the 5m cooldown -> no poll.
	cc.lastEventTS = time.Now().Add(-10 * time.Second)
	cc.maybePoll(context.Background())
	if mgr.callCount() != 0 {
		t.Fatalf("expected no poll within cooldown, got %d calls", mgr.callCount())
	}

	// Event 2m ago: past the window but still inside the 5m cooldown -> no poll.
	cc.lastEventTS = time.Now().Add(-2 * time.Minute)
	cc.maybePoll(context.Background())
	if mgr.callCount() != 0 {
		t.Fatalf("expected no poll within cooldown, got %d calls", mgr.callCount())
	}

	// Never-seen (zero) -> polls.
	cc.lastEventTS = time.Time{}
	cc.maybePoll(context.Background())
	if mgr.callCount() != 1 {
		t.Fatalf("expected a poll when no event seen, got %d calls", mgr.callCount())
	}

	// Event 6m ago: past the cooldown -> polls.
	cc.lastEventTS = time.Now().Add(-6 * time.Minute)
	cc.maybePoll(context.Background())
	if mgr.callCount() != 2 {
		t.Fatalf("expected a poll past cooldown, got %d calls", mgr.callCount())
	}
}

func TestConditional_fetchLatestEvent(t *testing.T) {
	// event_ts arrives as float64, mirroring gRPC/structpb decoding.
	now := time.Now().UnixMilli()
	resp := map[string]interface{}{
		"events": []interface{}{
			map[string]interface{}{"camera": "front_door", "event_ts": float64(now - 5000)},
			map[string]interface{}{"camera": "front_door", "event_ts": float64(now)},
			map[string]interface{}{"camera": "garage", "event_ts": float64(now + 10000)},
		},
	}

	t.Run("scoped to camera_name picks newest matching", func(t *testing.T) {
		cc := newTestCam(t, newFakeManager(resp, nil), oneImageCam(), 20*time.Second, 5*time.Minute, "front_door")
		ts, _, ok := cc.fetchLatestEvent(context.Background())
		if !ok {
			t.Fatal("expected an event")
		}
		if got := ts.UnixMilli(); got != now {
			t.Fatalf("want newest front_door ts %d, got %d (should ignore later garage event)", now, got)
		}
	})

	t.Run("unscoped picks newest overall", func(t *testing.T) {
		cc := newTestCam(t, newFakeManager(resp, nil), oneImageCam(), 20*time.Second, 5*time.Minute, "")
		ts, _, ok := cc.fetchLatestEvent(context.Background())
		if !ok {
			t.Fatal("expected an event")
		}
		if got := ts.UnixMilli(); got != now+10000 {
			t.Fatalf("want newest overall ts %d, got %d", now+10000, got)
		}
	})

	t.Run("returns event_id of newest matching", func(t *testing.T) {
		withIDs := map[string]interface{}{
			"events": []interface{}{
				map[string]interface{}{"camera": "front_door", "event_ts": float64(now - 5000), "event_id": "older"},
				map[string]interface{}{"camera": "front_door", "event_ts": float64(now), "event_id": "newest"},
			},
		}
		cc := newTestCam(t, newFakeManager(withIDs, nil), oneImageCam(), 20*time.Second, 5*time.Minute, "front_door")
		_, id, ok := cc.fetchLatestEvent(context.Background())
		if !ok {
			t.Fatal("expected an event")
		}
		if id != "newest" {
			t.Fatalf("want event_id %q, got %q", "newest", id)
		}
	})

	t.Run("no matching camera -> none", func(t *testing.T) {
		cc := newTestCam(t, newFakeManager(resp, nil), oneImageCam(), 20*time.Second, 5*time.Minute, "backyard")
		if _, _, ok := cc.fetchLatestEvent(context.Background()); ok {
			t.Fatal("expected no event for unknown camera")
		}
	})

	t.Run("manager error -> none", func(t *testing.T) {
		cc := newTestCam(t, newFakeManager(nil, errors.New("boom")), oneImageCam(), 20*time.Second, 5*time.Minute, "")
		if _, _, ok := cc.fetchLatestEvent(context.Background()); ok {
			t.Fatal("expected no event on manager error")
		}
	})
}

func TestConditional_Validate(t *testing.T) {
	t.Run("requires camera", func(t *testing.T) {
		_, _, err := (&ConditionalConfig{Manager: "mgr"}).Validate("p")
		if err == nil {
			t.Fatal("expected error when camera missing")
		}
	})
	t.Run("requires manager", func(t *testing.T) {
		_, _, err := (&ConditionalConfig{Camera: "cam"}).Validate("p")
		if err == nil {
			t.Fatal("expected error when manager missing")
		}
	})
	t.Run("rejects negative timings", func(t *testing.T) {
		_, _, err := (&ConditionalConfig{Camera: "cam", Manager: "mgr", WindowSeconds: -1}).Validate("p")
		if err == nil {
			t.Fatal("expected error for negative window_seconds")
		}
	})
	t.Run("valid returns deps", func(t *testing.T) {
		deps, _, err := (&ConditionalConfig{Camera: "cam", Manager: "mgr"}).Validate("p")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(deps) != 2 || deps[0] != "cam" || deps[1] != "mgr" {
			t.Fatalf("want deps [cam mgr], got %v", deps)
		}
	})
}
