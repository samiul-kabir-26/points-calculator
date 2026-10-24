// Command api runs the multi-tenant point calculation engine HTTP service.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/samiul-kabir-26/points-calculator/internal/api"
	"github.com/samiul-kabir-26/points-calculator/internal/config"
	"github.com/samiul-kabir-26/points-calculator/internal/database"
	"github.com/samiul-kabir-26/points-calculator/internal/engine"
	"github.com/samiul-kabir-26/points-calculator/internal/limiter"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Connect(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	if cfg.AutoMigrate {
		if err := database.Migrate(ctx, pool); err != nil {
			return err
		}
		log.Info("migrations applied")
	}

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	rdb := redis.NewClient(redisOpts)
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return err
	}

	st := store.New(pool)
	lim := limiter.New(rdb)
	srv := api.NewServer(api.Deps{
		Pool:          pool,
		Store:         st,
		Engine:        engine.New(st, lim, log),
		Limiter:       lim,
		PlatformToken: cfg.PlatformToken,
		Logger:        log,
	})

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if cfg.LevelSweepInterval > 0 {
		go sweepLevels(ctx, st, cfg.LevelSweepInterval, log)
	}
	if cfg.ExpirySweepInterval > 0 {
		go sweepExpiry(ctx, st, cfg.ExpirySweepInterval, log)
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return err
		}
	}
	return nil
}

// sweepLevels periodically applies level changes that depend only on time passing:
// spend ageing out of a rolling window, and downgrades whose grace period has ended.
func sweepLevels(ctx context.Context, st *store.Store, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ran, changed, err := st.SweepLevels(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.ErrorContext(ctx, "level sweep failed", "err", err, "changed", changed)
		case ran:
			log.InfoContext(ctx, "level sweep finished", "changed", changed)
		}
	}
}

// sweepExpiry periodically removes expired points from balances, so idle entities' balances
// catch up; an entity's own transactions expire its due points first anyway.
func sweepExpiry(ctx context.Context, st *store.Store, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		ran, entities, err := st.ExpirePoints(ctx, nil)
		switch {
		case err != nil && ctx.Err() == nil:
			log.ErrorContext(ctx, "expiry sweep failed", "err", err, "entities", entities)
		case ran && entities > 0:
			log.InfoContext(ctx, "expiry sweep finished", "entities", entities)
		}
	}
}
