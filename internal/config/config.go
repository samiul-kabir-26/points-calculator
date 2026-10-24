// Package config loads service configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	RedisURL        string
	PlatformToken   string
	AutoMigrate     bool
	DBMaxConns      int32
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	// LevelSweepInterval is how often time-based level changes are applied; 0 disables the sweep.
	LevelSweepInterval time.Duration
	// ExpirySweepInterval is how often expired points are removed from balances; 0 disables
	// the sweep (points still expire inside each entity's next transaction).
	ExpirySweepInterval time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		HTTPAddr:        getenv("HTTP_ADDR", ":8080"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		RedisURL:        getenv("REDIS_URL", "redis://localhost:6379/0"),
		PlatformToken:   os.Getenv("PLATFORM_ADMIN_TOKEN"),
		AutoMigrate:     true,
		DBMaxConns:      20,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 15 * time.Second,

		LevelSweepInterval:  time.Hour,
		ExpirySweepInterval: 15 * time.Minute,
	}

	var errs []error
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if cfg.PlatformToken == "" {
		errs = append(errs, errors.New("PLATFORM_ADMIN_TOKEN is required"))
	}
	if v := os.Getenv("AUTO_MIGRATE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("AUTO_MIGRATE: %w", err))
		}
		cfg.AutoMigrate = b
	}
	if v := os.Getenv("DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be a positive integer"))
		}
		cfg.DBMaxConns = int32(n)
	}
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(strings.ToUpper(v))); err != nil {
			errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
		}
	}
	if v := os.Getenv("SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err))
		}
		cfg.ShutdownTimeout = d
	}
	if v := os.Getenv("LEVEL_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("LEVEL_SWEEP_INTERVAL must be a non-negative duration"))
		}
		cfg.LevelSweepInterval = d
	}
	if v := os.Getenv("EXPIRY_SWEEP_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			errs = append(errs, fmt.Errorf("EXPIRY_SWEEP_INTERVAL must be a non-negative duration"))
		}
		cfg.ExpirySweepInterval = d
	}
	return cfg, errors.Join(errs...)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
