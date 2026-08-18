package viammod

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeGo2RTC is a scriptable stand-in for *go2rtcmgr.Manager.
type fakeGo2RTC struct {
	mu       sync.Mutex
	healthy  bool
	starts   int
	stops    int
	startErr error
	readyErr error
}

func (f *fakeGo2RTC) IsHealthy(context.Context) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.healthy
}

func (f *fakeGo2RTC) Start(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.startErr != nil {
		return f.startErr
	}
	f.healthy = true
	return nil
}

func (f *fakeGo2RTC) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.healthy = false
	return nil
}

func (f *fakeGo2RTC) WaitReady(context.Context, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readyErr
}

func (f *fakeGo2RTC) setHealthy(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthy = v
}

func (f *fakeGo2RTC) counts() (starts, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

// fastSupervisor shrinks the timers so tests run in milliseconds. The spawn
// hook hands back the same fake (a fake never wedges the way a real Manager
// can) and counts calls, so tests can assert each restart asks for a fresh one.
func fastSupervisor(proc go2rtcProcess, onRestart func(context.Context)) (*go2rtcSupervisor, *atomic.Int32) {
	spawns := &atomic.Int32{}
	s := newGo2RTCSupervisor(proc, func() go2rtcProcess {
		spawns.Add(1)
		return proc
	}, onRestart, zerolog.Nop())
	s.poll = 5 * time.Millisecond
	s.backoff = time.Millisecond
	s.maxBackoff = 5 * time.Millisecond
	s.readyTimeout = 50 * time.Millisecond
	return s, spawns
}

func TestGo2RTCSupervisor_RestartsWhenUnhealthy(t *testing.T) {
	proc := &fakeGo2RTC{healthy: true}
	restarted := make(chan struct{}, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup, spawns := fastSupervisor(proc, func(context.Context) { restarted <- struct{}{} })
	go sup.Run(ctx)

	proc.setHealthy(false)

	select {
	case <-restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("no restart after go2rtc went unhealthy")
	}

	starts, stops := proc.counts()
	if starts != 1 || stops != 1 {
		t.Errorf("starts=%d stops=%d, want 1/1", starts, stops)
	}
	if n := spawns.Load(); n != 1 {
		t.Errorf("spawns = %d, want 1 fresh manager per restart", n)
	}
}

func TestGo2RTCSupervisor_HealthyIsLeftAlone(t *testing.T) {
	proc := &fakeGo2RTC{healthy: true}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	sup, _ := fastSupervisor(proc, func(context.Context) {
		t.Error("onRestart fired for a healthy go2rtc")
	})
	sup.Run(ctx)

	if starts, stops := proc.counts(); starts != 0 || stops != 0 {
		t.Errorf("starts=%d stops=%d, want 0/0", starts, stops)
	}
}

func TestGo2RTCSupervisor_SingleMissDoesNotRestart(t *testing.T) {
	proc := &fakeGo2RTC{healthy: true}
	sup, _ := fastSupervisor(proc, func(context.Context) {
		t.Error("onRestart fired on a single missed health check")
	})
	sup.strikes = 3

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	// One poll's worth of unhealthy, then recover before the strike count.
	proc.setHealthy(false)
	time.Sleep(8 * time.Millisecond)
	proc.setHealthy(true)
	time.Sleep(50 * time.Millisecond)

	if starts, stops := proc.counts(); starts != 0 || stops != 0 {
		t.Errorf("starts=%d stops=%d, want 0/0", starts, stops)
	}
}

func TestGo2RTCSupervisor_RetriesFailedRestart(t *testing.T) {
	// Port still held / binary missing: Start keeps failing, so the supervisor
	// must retry rather than give up, and must not report a restart.
	proc := &fakeGo2RTC{healthy: false, startErr: errors.New("port 1984 in use")}
	sup, spawns := fastSupervisor(proc, func(context.Context) {
		t.Error("onRestart fired despite a failed restart")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Each retry must ask spawn for a fresh manager: a real one whose exec
		// failed is permanently unusable.
		if starts, _ := proc.counts(); starts >= 3 && spawns.Load() >= 3 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	starts, _ := proc.counts()
	t.Fatalf("starts=%d spawns=%d, want >= 3 retries each", starts, spawns.Load())
}

func TestGo2RTCSupervisor_StopsUnreadyProcess(t *testing.T) {
	// Process starts but never answers: it must be stopped again so it doesn't
	// hold the ports, and no restart reported.
	proc := &fakeGo2RTC{healthy: false, readyErr: errors.New("not ready")}
	sup, _ := fastSupervisor(proc, func(context.Context) {
		t.Error("onRestart fired for a go2rtc that never became ready")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if starts, stops := proc.counts(); starts >= 1 && stops >= starts*2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	starts, stops := proc.counts()
	t.Fatalf("starts=%d stops=%d, want each start followed by a stop", starts, stops)
}

func TestGo2RTCSupervisor_ContextCancelReturns(t *testing.T) {
	proc := &fakeGo2RTC{healthy: true}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		sup, _ := fastSupervisor(proc, nil)
		sup.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

func TestGo2RTCSupervisor_StopStopsCurrentProcess(t *testing.T) {
	// The Close path: cancel first, then Stop.
	proc := &fakeGo2RTC{healthy: true}
	sup, _ := fastSupervisor(proc, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Run(ctx); close(done) }()

	cancel()
	<-done
	if err := sup.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, stops := proc.counts(); stops != 1 {
		t.Errorf("stops = %d, want 1", stops)
	}
	if proc.IsHealthy(ctx) {
		t.Error("process still healthy after Stop")
	}
}
