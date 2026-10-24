// Package api exposes the HTTP interface: platform provisioning, the tenant
// admin API, the execution engine, and the query/log API.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samiul-kabir-26/points-calculator/internal/engine"
	"github.com/samiul-kabir-26/points-calculator/internal/limiter"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

type Server struct {
	store         *store.Store
	engine        *engine.Engine
	limiter       *limiter.Limiter
	pool          *pgxpool.Pool
	platformToken string
	log           *slog.Logger
}

type Deps struct {
	Pool          *pgxpool.Pool
	Store         *store.Store
	Engine        *engine.Engine
	Limiter       *limiter.Limiter
	PlatformToken string
	Logger        *slog.Logger
}

func NewServer(d Deps) *Server {
	return &Server{
		store:         d.Store,
		engine:        d.Engine,
		limiter:       d.Limiter,
		pool:          d.Pool,
		platformToken: d.PlatformToken,
		log:           d.Logger,
	}
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	t := s.requireTenant

	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)

	// Admin console.
	mux.HandleFunc("GET /ui/", s.ui)
	mux.Handle("GET /{$}", http.RedirectHandler("/ui/", http.StatusFound))

	// Platform operator: tenant onboarding.
	mux.HandleFunc("POST /platform/tenants", s.requirePlatform(s.createTenant))

	// Management / admin API.
	mux.HandleFunc("GET /admin/architecture", t(s.getArchitecture))
	mux.HandleFunc("POST /admin/architecture", t(s.setArchitecture))

	mux.HandleFunc("GET /admin/settings", t(s.getSettings))
	mux.HandleFunc("PATCH /admin/settings", t(s.updateSettings))

	mux.HandleFunc("GET /admin/api-keys", t(s.listAPIKeys))
	mux.HandleFunc("POST /admin/api-keys", t(s.createAPIKey))
	mux.HandleFunc("DELETE /admin/api-keys/{key_id}", t(s.revokeAPIKey))

	mux.HandleFunc("POST /admin/point-types", t(s.createPointType))
	mux.HandleFunc("GET /admin/point-types", t(s.listPointTypes))
	mux.HandleFunc("GET /admin/point-types/{point_type_id}", t(s.getPointType))
	mux.HandleFunc("PATCH /admin/point-types/{point_type_id}", t(s.updatePointType))
	mux.HandleFunc("DELETE /admin/point-types/{point_type_id}", t(s.deletePointType))

	mux.HandleFunc("POST /admin/tiers", t(s.createTier))
	mux.HandleFunc("GET /admin/tiers", t(s.listTiers))
	mux.HandleFunc("GET /admin/tiers/{tier_id}", t(s.getTier))
	mux.HandleFunc("PATCH /admin/tiers/{tier_id}", t(s.updateTier))
	mux.HandleFunc("DELETE /admin/tiers/{tier_id}", t(s.deleteTier))
	mux.HandleFunc("PATCH /admin/tiers/{tier_id}/status", t(s.setTierStatus))

	mux.HandleFunc("POST /admin/tiers/{tier_id}/rules", t(s.createRule))
	mux.HandleFunc("GET /admin/tiers/{tier_id}/rules", t(s.listRules))
	mux.HandleFunc("GET /admin/tiers/{tier_id}/rules/{rule_id}", t(s.getRule))
	mux.HandleFunc("PATCH /admin/tiers/{tier_id}/rules/{rule_id}", t(s.updateRule))
	mux.HandleFunc("DELETE /admin/tiers/{tier_id}/rules/{rule_id}", t(s.deleteRule))
	mux.HandleFunc("PATCH /admin/tiers/{tier_id}/rules/{rule_id}/status", t(s.setRuleStatus))

	mux.HandleFunc("GET /admin/level-policy", t(s.getLevelPolicy))
	mux.HandleFunc("PATCH /admin/level-policy", t(s.updateLevelPolicy))

	mux.HandleFunc("POST /admin/levels", t(s.createLevel))
	mux.HandleFunc("GET /admin/levels", t(s.listLevels))
	mux.HandleFunc("POST /admin/levels/reevaluate", t(s.reevaluateLevels))
	mux.HandleFunc("GET /admin/levels/{level_id}", t(s.getLevel))
	mux.HandleFunc("PATCH /admin/levels/{level_id}", t(s.updateLevel))
	mux.HandleFunc("DELETE /admin/levels/{level_id}", t(s.deleteLevel))
	mux.HandleFunc("PATCH /admin/levels/{level_id}/status", t(s.setLevelStatus))

	mux.HandleFunc("POST /admin/adjustments", t(s.createAdjustment))

	// Execution API.
	mux.HandleFunc("POST /engine/execute", t(s.execute))

	// Query & log API.
	mux.HandleFunc("GET /data/profiles", t(s.listProfiles))
	mux.HandleFunc("GET /data/profiles/{entity_id}", t(s.getProfile))
	mux.HandleFunc("GET /data/profiles/{entity_id}/logs", t(s.listEntityLogs))
	mux.HandleFunc("GET /data/profiles/{entity_id}/level-history", t(s.listLevelHistory))
	mux.HandleFunc("GET /data/profiles/{entity_id}/expirations", t(s.listExpirations))
	mux.HandleFunc("GET /data/logs", t(s.listLogs))

	return s.withRequestContext(mux)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether both backing stores are reachable.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"postgres": "ok", "redis": "ok"}
	status := http.StatusOK
	if err := s.pool.Ping(ctx); err != nil {
		checks["postgres"], status = err.Error(), http.StatusServiceUnavailable
	}
	if err := s.limiter.Ping(ctx); err != nil {
		checks["redis"], status = err.Error(), http.StatusServiceUnavailable
	}
	writeJSON(w, status, checks)
}
