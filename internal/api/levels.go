package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

// ---- Level policy ----

type levelPolicyRequest struct {
	Basis         *string `json:"basis"`
	WindowDays    *int    `json:"window_days"`
	PeriodMonths  *int    `json:"period_months"`
	Downgrade     *bool   `json:"downgrade"`
	GraceDays     *int    `json:"grace_days"`
	MinTenureDays *int    `json:"min_tenure_days"`
}

func (s *Server) getLevelPolicy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, tenantFrom(r.Context()).LevelPolicy)
}

func (s *Server) updateLevelPolicy(w http.ResponseWriter, r *http.Request) {
	var req levelPolicyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.store.UpdateLevelPolicy(r.Context(), tenantFrom(r.Context()).ID, func(p *store.LevelPolicy) error {
		if req.Basis != nil {
			p.Basis = *req.Basis
			// Switching basis drops the other basis's setting instead of making the caller clear it.
			if p.Basis != store.LevelBasisRolling && req.WindowDays == nil {
				p.WindowDays = nil
			}
			if p.Basis != store.LevelBasisCalendar && req.PeriodMonths == nil {
				p.PeriodMonths = nil
			}
		}
		if req.WindowDays != nil {
			p.WindowDays = req.WindowDays
		}
		if req.PeriodMonths != nil {
			p.PeriodMonths = req.PeriodMonths
		}
		set(&p.Downgrade, req.Downgrade)
		set(&p.GraceDays, req.GraceDays)
		set(&p.MinTenureDays, req.MinTenureDays)
		return validateLevelPolicy(*p)
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ---- Levels ----

// nullableUUID tells an absent field (Set false) apart from an explicit null.
type nullableUUID struct {
	Set   bool
	Value *uuid.UUID
}

func (n *nullableUUID) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var id uuid.UUID
	if err := json.Unmarshal(b, &id); err != nil {
		return err
	}
	n.Value = &id
	return nil
}

type levelRequest struct {
	Slug        *string `json:"slug"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Rank        *int    `json:"rank"`
	MinSpend    *int64  `json:"min_spend"`
	MinOrders   *int64  `json:"min_orders"`
	// MinPoints replaces the level's per-point-type minimums, keyed by point type slug.
	MinPoints *map[string]int64 `json:"min_points"`
	TierID    nullableUUID      `json:"tier_id"`
	IsActive  *bool             `json:"is_active"`
}

func (req levelRequest) apply(l *store.Level) {
	set(&l.Slug, req.Slug)
	set(&l.Name, req.Name)
	set(&l.Description, req.Description)
	set(&l.Rank, req.Rank)
	set(&l.MinSpend, req.MinSpend)
	set(&l.MinOrders, req.MinOrders)
	if req.MinPoints != nil {
		l.MinPoints = *req.MinPoints
	}
	if req.TierID.Set {
		l.TierID = req.TierID.Value
	}
	set(&l.IsActive, req.IsActive)
}

func (s *Server) createLevel(w http.ResponseWriter, r *http.Request) {
	var req levelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.Rank == nil {
		s.writeError(w, r, apperr.New(http.StatusBadRequest, "validation_failed",
			"rank is required: it orders the levels, higher is better"))
		return
	}
	l := store.Level{TenantID: tenantFrom(r.Context()).ID, IsActive: true}
	req.apply(&l)
	if err := validateLevel(l); err != nil {
		s.writeError(w, r, err)
		return
	}
	l, err := s.store.CreateLevel(r.Context(), l)
	if err != nil {
		s.writeError(w, r, levelErr(err))
		return
	}
	writeJSON(w, http.StatusCreated, l)
}

func (s *Server) listLevels(w http.ResponseWriter, r *http.Request) {
	levels, err := s.store.ListLevels(r.Context(), tenantFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": levels})
}

func (s *Server) getLevel(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "level_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	l, err := s.store.GetLevel(r.Context(), tenantFrom(r.Context()).ID, id)
	if err != nil {
		s.writeError(w, r, levelErr(err))
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) updateLevel(w http.ResponseWriter, r *http.Request) {
	var req levelRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.patchLevel(w, r, req)
}

func (s *Server) setLevelStatus(w http.ResponseWriter, r *http.Request) {
	active, err := decodeStatus(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.patchLevel(w, r, levelRequest{IsActive: &active})
}

func (s *Server) patchLevel(w http.ResponseWriter, r *http.Request, req levelRequest) {
	id, err := pathUUID(r, "level_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	l, err := s.store.UpdateLevel(r.Context(), tenantFrom(r.Context()).ID, id, func(l *store.Level) error {
		req.apply(l)
		return validateLevel(*l)
	})
	if err != nil {
		s.writeError(w, r, levelErr(err))
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (s *Server) deleteLevel(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "level_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.DeleteLevel(r.Context(), tenantFrom(r.Context()).ID, id); err != nil {
		s.writeError(w, r, levelErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reevaluateLevels re-evaluates every entity of the tenant now, e.g. after thresholds change.
func (s *Server) reevaluateLevels(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	evaluated, changed, err := s.store.ReevaluateLevels(r.Context(), tenant, nil, false)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"evaluated": evaluated, "changed": changed})
}

func levelErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("level_not_found", "level not found")
	case errors.Is(err, store.ErrConflict):
		return apperr.Conflict("level_slug_taken", "a level with that slug already exists")
	case errors.Is(err, store.ErrLevelRankTaken):
		return apperr.Conflict("level_rank_taken", "another level already uses that rank")
	case errors.Is(err, store.ErrLevelTierNotFound):
		return apperr.NotFound("tier_not_found", "tier not found")
	}
	return pointTypeErr(err)
}

// listLevelHistory handles GET /data/profiles/{entity_id}/level-history?limit=&cursor=&order=.
func (s *Server) listLevelHistory(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	entityID := r.PathValue("entity_id")
	if _, err := s.store.GetProfile(r.Context(), tenant.ID, entityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = apperr.NotFound("profile_not_found", "no profile exists for that entity")
		}
		s.writeError(w, r, err)
		return
	}
	limit, err := parseLimit(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	asc, err := parseOrder(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f := store.LevelChangeFilter{EntityID: entityID, Limit: limit + 1, Ascending: asc}
	if c := r.URL.Query().Get("cursor"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			s.writeError(w, r, invalidCursor())
			return
		}
		f.Cursor = &n
	}
	changes, err := s.store.ListLevelChanges(r.Context(), tenant.ID, f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := page[store.LevelChange]{Data: changes}
	if len(changes) > limit {
		out.Data = changes[:limit]
		out.NextCursor = cursorString(changes[limit-1].Sequence)
	}
	writeJSON(w, http.StatusOK, out)
}
