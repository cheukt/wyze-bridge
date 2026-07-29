// wyze-headless is a stripped-down variant of wyze-bridge: Wyze auth +
// camera discovery + go2rtc streaming, with no WebUI, MQTT, webhooks,
// recording, or snapshots. It targets TUTK cameras with an embedded
// go2rtc subprocess.
//
// Config is loaded from a dotenv file (path from ENV_FILE, default
// .env.dev) into the process env, then assembled into a *config.Config.
// The loadConfig() seam is meant to be swapped later for a directly
// constructed / flag-driven config without touching the rest of main.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/envfile"
	"github.com/IDisposable/docker-wyze-bridge/internal/go2rtcmgr"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

// Version is set at build time via ldflags.
var Version = "4.0-beta-headless"

// defaultEnvFile is loaded when ENV_FILE is unset. A missing file at this
// path is non-fatal; a missing file at an explicit ENV_FILE is an error.
const defaultEnvFile = ".env.dev"

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	initLogging(cfg)

	log.Info().
		Str("version", Version).
		Str("log_level", cfg.LogLevel.String()).
		Msg("starting wyze-headless")

	// Context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Info().Str("signal", sig.String()).Msg("shutdown signal received")
		cancel()
	}()

	// Wyze API client (stateless — re-authenticates on every start).
	apiLog := log.With().Str("c", "wyzeapi").Logger()
	creds := wyzeapi.Credentials{
		Email:    cfg.WyzeEmail,
		Password: cfg.WyzePassword,
		APIID:    cfg.WyzeAPIID,
		APIKey:   cfg.WyzeAPIKey,
		TOTPKey:  cfg.WyzeTOTPKey,
	}
	apiClient := wyzeapi.NewClient(creds, Version, apiLog)

	camLog := log.With().Str("c", "camera").Logger()
	camMgr := camera.NewManager(cfg, apiClient, nil, camLog)

	go2rtcLog := log.With().Str("c", "go2rtc").Logger()
	go2rtcAPI, go2rtcMgr := setupGo2RTC(ctx, cfg, go2rtcLog)

	camMgr.SetGo2RTCAPI(go2rtcAPI)
	go camMgr.RunDiscoveryLoop(ctx)

	<-ctx.Done()
	log.Info().Msg("shutting down")
	if go2rtcMgr != nil {
		if err := go2rtcMgr.Stop(); err != nil {
			log.Error().Err(err).Msg("stop go2rtc manager")
		}
	}
	log.Info().Msg("goodbye")
}

// loadConfig loads the dotenv file (ENV_FILE, default .env.dev) into the
// process env, then builds a *config.Config from env. This is the seam to
// replace later with a directly constructed / flag-driven config.
func loadConfig() (*config.Config, error) {
	path, explicit := os.LookupEnv("ENV_FILE")
	if !explicit {
		path = defaultEnvFile
	}
	if err := loadEnvFile(path, explicit); err != nil {
		return nil, err
	}

	cfg := &config.Config{
		// Wyze credentials (required)
		WyzeEmail:    os.Getenv("WYZE_EMAIL"),
		WyzePassword: os.Getenv("WYZE_PASSWORD"),
		WyzeAPIID:    os.Getenv("WYZE_API_ID"),
		WyzeAPIKey:   os.Getenv("WYZE_API_KEY"),
		WyzeTOTPKey:  os.Getenv("WYZE_TOTP_KEY"),

		// Env-driven knobs (testing convenience)
		BridgeIP:        os.Getenv("BRIDGE_IP"),
		StateDir:        envOr("STATE_DIR", "./local/config"),
		LogLevel:        config.ParseLogLevel(os.Getenv("LOG_LEVEL")),
		ForceIOTCDetail: envBool("FORCE_IOTC_DETAIL"),

		// Literals for the TUTK + embedded-go2rtc core.
		RefreshInterval: 30 * time.Minute, // must be non-zero (ticker)
		Quality:         "hd",
		Audio:           true,
		STUNServer:      "stun:stun.l.google.com:19302",
		BridgePort:      5080,
		GwellEnabled:    false,
		CamOverrides:    make(map[string]config.CamOverride),
	}

	creds := wyzeapi.Credentials{
		Email:    cfg.WyzeEmail,
		Password: cfg.WyzePassword,
		APIID:    cfg.WyzeAPIID,
		APIKey:   cfg.WyzeAPIKey,
	}
	if !creds.IsSet() {
		return nil, fmt.Errorf("missing required Wyze credentials (WYZE_EMAIL, WYZE_PASSWORD, WYZE_API_ID, WYZE_API_KEY) — set them in %s or the environment", path)
	}

	return cfg, nil
}

// loadEnvFile parses a dotenv file and sets each key via os.Setenv, but
// only when the key isn't already present in the environment, so a real
// shell env var wins over the file. A missing file is an error only when
// the path was set explicitly via ENV_FILE.
func loadEnvFile(path string, explicit bool) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return nil // default file is optional
		}
		return fmt.Errorf("open env file %q: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		key, val, ok := envfile.ParseLine(scanner.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); set {
			continue // real env wins
		}
		os.Setenv(key, val)
	}
	return scanner.Err()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func initLogging(cfg *config.Config) {
	if isatty.IsTerminal(os.Stdout.Fd()) {
		output := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}
		log.Logger = zerolog.New(output).With().Timestamp().Logger()
	} else {
		log.Logger = zerolog.New(os.Stdout).With().Timestamp().Logger()
	}
	zerolog.SetGlobalLevel(cfg.LogLevel)

	if cfg.ForceIOTCDetail {
		zerolog.SetGlobalLevel(zerolog.TraceLevel)
	}
}

// setupGo2RTC spawns and manages an embedded go2rtc subprocess. Stream
// registrations happen later via the HTTP API as cameras connect (see
// camera.Manager.ConnectAll). Adapted from the embedded branch of
// cmd/wyze-bridge's setupGo2RTC — the external GO2RTC_URL mode is dropped.
func setupGo2RTC(ctx context.Context, cfg *config.Config, go2rtcLog zerolog.Logger) (*go2rtcmgr.APIClient, *go2rtcmgr.Manager) {
	logLevel := "warn"
	if cfg.ForceIOTCDetail {
		logLevel = "debug"
	}
	configBuilder := go2rtcmgr.NewConfigBuilder(logLevel, cfg.STUNServer, cfg.BridgeIP)

	if cfg.StreamAuth != "" {
		entries := go2rtcmgr.ParseStreamAuth(cfg.StreamAuth)
		configBuilder.SetStreamAuth(entries)
		log.Info().Int("users", len(entries)).Msg("STREAM_AUTH configured")
	}

	go2rtcConfigPath := cfg.StateDir + "/go2rtc.yaml"
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		log.Fatal().Err(err).Str("dir", cfg.StateDir).Msg("cannot create state dir")
	}
	if err := configBuilder.WriteConfig(go2rtcConfigPath); err != nil {
		log.Fatal().Err(err).Msg("write go2rtc config")
	}

	go2rtcBinary := findGo2RTCBinary()
	mgr := go2rtcmgr.NewManager(go2rtcBinary, go2rtcConfigPath, go2rtcLog)

	if err := mgr.Start(ctx); err != nil {
		log.Fatal().Err(err).Msg("start go2rtc")
	}

	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	if err := mgr.WaitReady(readyCtx, 10*time.Second); err != nil {
		log.Fatal().Err(err).Msg("go2rtc not ready")
	}

	go2rtcAPI := go2rtcmgr.NewAPIClient(mgr.APIURL(), go2rtcLog)
	return go2rtcAPI, mgr
}

func findGo2RTCBinary() string {
	paths := []string{
		"./go2rtc",     // local dev (current dir)
		"./go2rtc.exe", // local dev (Windows)
		"/usr/local/bin/go2rtc",
		"/usr/bin/go2rtc",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "go2rtc" // fall back to PATH lookup
}
