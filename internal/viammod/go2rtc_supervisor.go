package viammod

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
)

// Two strikes at a 5s poll: a dead go2rtc is caught in ~10s, a single busy-API
// blip isn't mistaken for one.
const (
	go2rtcPollInterval     = 5 * time.Second
	go2rtcUnhealthyStrikes = 2
	go2rtcInitialBackoff   = 2 * time.Second
	go2rtcMaxBackoff       = 60 * time.Second
	go2rtcReadyTimeout     = 10 * time.Second
)

// go2rtcProcess is the slice of *go2rtcmgr.Manager the supervisor uses, as an
// interface so tests don't spawn subprocesses.
type go2rtcProcess interface {
	IsHealthy(ctx context.Context) bool
	Start(ctx context.Context) error
	Stop() error
	WaitReady(ctx context.Context, timeout time.Duration) error
}

// go2rtcSupervisor restarts go2rtc when it stops answering.
//
// It lives here rather than in go2rtcmgr because that package is upstream-owned
// and every line added there is a merge conflict on the next pull. So it polls
// the exported health endpoint instead of watching the (unexported) process exit
// channel: slower to notice a crash, but it also catches an alive-but-wedged
// go2rtc that an exit watcher would miss.
type go2rtcSupervisor struct {
	spawn     func() go2rtcProcess  // fresh process per restart; see restart
	onRestart func(context.Context) // re-register runtime streams
	log       zerolog.Logger

	mu   sync.Mutex
	proc go2rtcProcess

	poll         time.Duration
	strikes      int
	backoff      time.Duration
	maxBackoff   time.Duration
	readyTimeout time.Duration
}

func newGo2RTCSupervisor(proc go2rtcProcess, spawn func() go2rtcProcess, onRestart func(context.Context), log zerolog.Logger) *go2rtcSupervisor {
	return &go2rtcSupervisor{
		spawn:        spawn,
		onRestart:    onRestart,
		log:          log,
		proc:         proc,
		poll:         go2rtcPollInterval,
		strikes:      go2rtcUnhealthyStrikes,
		backoff:      go2rtcInitialBackoff,
		maxBackoff:   go2rtcMaxBackoff,
		readyTimeout: go2rtcReadyTimeout,
	}
}

// Run blocks until ctx is cancelled. Call it in a goroutine once go2rtc is up.
// Close cancels the service context before stopping go2rtc, so a deliberate
// shutdown ends the loop instead of reading as a crash.
func (s *go2rtcSupervisor) Run(ctx context.Context) {
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()

	backoff := s.backoff
	misses := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if s.current().IsHealthy(ctx) {
			misses, backoff = 0, s.backoff
			continue
		}

		misses++
		if misses < s.strikes {
			s.log.Debug().Int("misses", misses).Msg("go2rtc health check failed")
			continue
		}

		s.log.Warn().Int("misses", misses).Dur("backoff", backoff).Msg("go2rtc unresponsive; restarting")
		if !sleepCtx(ctx, backoff) {
			return
		}

		if err := s.restart(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			// Retryable — a busy :1984 usually means the old process or an
			// orphan still holds it. Strikes stay latched so the next tick
			// tries again after a longer wait.
			s.log.Error().Err(err).Msg("go2rtc restart failed")
			backoff = min(backoff*2, s.maxBackoff)
			continue
		}

		misses, backoff = 0, s.backoff
		s.log.Info().Msg("go2rtc restarted")

		// A fresh go2rtc only knows the streams in its YAML; everything
		// registered at runtime over the HTTP API is gone.
		if s.onRestart != nil {
			s.onRestart(ctx)
		}
	}
}

// restart stops the old process, then brings up a new one on a *fresh* manager.
// Stop first is what makes the old process release :1984 before the new one's
// port pre-flight runs. The fresh manager sidesteps two dead ends in reusing the
// old one: its cmd stays non-nil if exec fails (every later Start then reports
// "already running"), and its ready channel is closed once WaitReady succeeds.
func (s *go2rtcSupervisor) restart(ctx context.Context) error {
	if err := s.current().Stop(); err != nil {
		s.log.Warn().Err(err).Msg("stopping unresponsive go2rtc")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	next := s.spawn()
	if err := next.Start(ctx); err != nil {
		return fmt.Errorf("start: %w", err)
	}

	readyCtx, cancel := context.WithTimeout(ctx, s.readyTimeout)
	defer cancel()
	if err := next.WaitReady(readyCtx, s.readyTimeout); err != nil {
		_ = next.Stop() // don't leave a wedged process holding the ports
		return fmt.Errorf("not ready: %w", err)
	}
	if ctx.Err() != nil {
		_ = next.Stop() // shut down while we were starting up
		return ctx.Err()
	}

	s.mu.Lock()
	s.proc = next
	s.mu.Unlock()
	return nil
}

// Stop shuts down whichever process is current. Cancel the supervisor's context
// first, or Run will treat the stop as a crash and restart it.
func (s *go2rtcSupervisor) Stop() error {
	return s.current().Stop()
}

func (s *go2rtcSupervisor) current() go2rtcProcess {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proc
}

// reregisterStreams pushes every camera's stream back into go2rtc after a
// restart. RestartStream is the exported per-camera path; ConnectAll would skip
// cameras still believing they stream. Parallel because with the health probe
// on, each reconnect waits for a real frame.
func reregisterStreams(ctx context.Context, camMgr *camera.Manager, log zerolog.Logger) {
	cams := camMgr.Cameras()
	if len(cams) == 0 {
		return
	}
	log.Info().Int("cameras", len(cams)).Msg("re-registering streams after go2rtc restart")

	var wg sync.WaitGroup
	for _, cam := range cams {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			camMgr.RestartStream(ctx, name)
		}(cam.Name())
	}
	wg.Wait()
}

// sleepCtx waits for d, reporting false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
