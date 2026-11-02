package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	token          string
	allowedUsers   map[int64]struct{}
	stateDir       string
	tmpDir         string
	maxUploadBytes int64
	fxtwitterBase  string
	pollTimeout    time.Duration
	timing         timingMode
}

const (
	heartbeatFile = "heartbeat"
	offsetFile    = "last_update_id"
	staleTempAge  = 24 * time.Hour
)

func main() {
	health := flag.Bool("healthcheck", false, "check bot heartbeat and exit")
	flag.Parse()

	if *health {
		os.Exit(healthcheck())
	}

	log := newLogger()

	cfg, err := loadConfig()
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Error("bot stopped", "err", redact(err, cfg.token))
		os.Exit(1)
	}
	log.Info("bot stopped gracefully")
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(envOr("LOG_LEVEL", "info")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func loadConfig() (config, error) {
	var cfg config

	cfg.token = strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	if cfg.token == "" {
		return cfg, errors.New("BOT_TOKEN is required")
	}

	rawUsers := strings.TrimSpace(os.Getenv("ALLOWED_USER_IDS"))
	if rawUsers == "" {
		return cfg, errors.New("ALLOWED_USER_IDS is required (comma-separated Telegram user IDs)")
	}
	cfg.allowedUsers = make(map[int64]struct{})
	for _, part := range strings.Split(rawUsers, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return cfg, fmt.Errorf("invalid user id %q in ALLOWED_USER_IDS", part)
		}
		cfg.allowedUsers[id] = struct{}{}
	}
	if len(cfg.allowedUsers) == 0 {
		return cfg, errors.New("ALLOWED_USER_IDS contains no valid ids")
	}

	cfg.stateDir = envOr("STATE_DIR", "/state")
	cfg.tmpDir = envOr("TMP_DIR", filepath.Join(cfg.stateDir, "tmp"))

	maxMB, err := strconv.ParseInt(envOr("MAX_UPLOAD_MB", "50"), 10, 64)
	if err != nil || maxMB < 1 || maxMB > 50 {
		return cfg, errors.New("MAX_UPLOAD_MB must be an integer between 1 and 50")
	}
	cfg.maxUploadBytes = maxMB << 20

	cfg.fxtwitterBase = strings.TrimRight(envOr("FXTWITTER_BASE", "https://api.fxtwitter.com"), "/")

	pollSeconds, err := strconv.Atoi(envOr("POLL_TIMEOUT_SECONDS", "60"))
	if err != nil || pollSeconds < 5 || pollSeconds > 120 {
		return cfg, errors.New("POLL_TIMEOUT_SECONDS must be an integer between 5 and 120")
	}
	cfg.pollTimeout = time.Duration(pollSeconds) * time.Second

	timing, err := parseTimingMode(envOr("TIMING_TRACE", "off"))
	if err != nil {
		return cfg, err
	}
	cfg.timing = timing

	for _, dir := range []string{cfg.stateDir, cfg.tmpDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return cfg, fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	return cfg, nil
}

func healthcheck() int {
	data, err := os.ReadFile(filepath.Join(envOr("STATE_DIR", "/state"), heartbeatFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: read heartbeat: %v\n", err)
		return 1
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: invalid heartbeat: %v\n", err)
		return 1
	}
	age := time.Since(time.Unix(ts, 0))
	if age > 3*time.Minute {
		fmt.Fprintf(os.Stderr, "healthcheck: heartbeat is stale (%s)\n", age.Round(time.Second))
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func redact(err error, secret string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, "<token>")
	}
	return msg
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
