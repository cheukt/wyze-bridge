package viammod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/go2rtcmgr"
)

// setupGo2RTC spawns and manages an embedded go2rtc subprocess. Ported from
// cmd/wyze-headless setupGo2RTC, but module-safe: it returns errors instead of
// calling log.Fatal (a module must never os.Exit), uses the injected logger,
// and threads the configured RTSP port through the config builder.
func setupGo2RTC(ctx context.Context, cfg *config.Config, rtspPort int, log zerolog.Logger) (*go2rtcmgr.APIClient, *go2rtcmgr.Manager, error) {
	logLevel := "warn"
	if cfg.ForceIOTCDetail {
		logLevel = "debug"
	}
	configBuilder := go2rtcmgr.NewConfigBuilder(logLevel, cfg.STUNServer, cfg.BridgeIP)
	configBuilder.SetRTSPPort(rtspPort)

	go2rtcConfigPath := filepath.Join(cfg.StateDir, "go2rtc.yaml")
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create state dir %q: %w", cfg.StateDir, err)
	}
	if err := configBuilder.WriteConfig(go2rtcConfigPath); err != nil {
		return nil, nil, fmt.Errorf("write go2rtc config: %w", err)
	}

	go2rtcBinary := findGo2RTCBinary()
	mgr := go2rtcmgr.NewManager(go2rtcBinary, go2rtcConfigPath, log)

	if err := mgr.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start go2rtc: %w", err)
	}

	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := mgr.WaitReady(readyCtx, 10*time.Second); err != nil {
		// Best-effort stop so a failed start doesn't leak the subprocess.
		_ = mgr.Stop()
		return nil, nil, fmt.Errorf("go2rtc not ready: %w", err)
	}

	api := go2rtcmgr.NewAPIClient(mgr.APIURL(), log)
	return api, mgr, nil
}

// findGo2RTCBinary locates the bundled go2rtc binary. It first probes the
// directory of the running module executable (where `make module.tar.gz`
// ships go2rtc next to bin/viam-module), then the headless dev/system paths,
// and finally falls back to a PATH lookup.
func findGo2RTCBinary() string {
	var paths []string
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		paths = append(paths, filepath.Join(dir, "go2rtc"), filepath.Join(dir, "go2rtc.exe"))
	}
	paths = append(paths,
		"./go2rtc",     // local dev (current dir)
		"./go2rtc.exe", // local dev (Windows)
		"/usr/local/bin/go2rtc",
		"/usr/bin/go2rtc",
	)
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "go2rtc" // fall back to PATH lookup
}
