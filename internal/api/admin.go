package api

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/engine"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

// ---- Platform: tenant onboarding ----

type createTenantRequest struct {
	Name            string  `json:"name"`
	ArchitectureKey *string `json:"architecture_key"`
	// Timezone defaults to UTC.
	Timezone *string `json:"timezone"`
}

type createTenantResponse struct {
	Tenant store.Tenant `json:"tenant"`
	APIKey newKeyBody   `json:"api_key"`
}

type newKeyBody struct {
	store.APIKey
	// Secret is only ever returned at creation time.
	Secret string `json:"secret"`
}

func (s *Server) createTenant(w http.ResponseWriter, r *http.Request) {
	var req createTenantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var f fieldErrors
	f.text("name", req.Name, true, maxNameLen)
	if req.ArchitectureKey != nil && !archKeyPattern.MatchString(*req.ArchitectureKey) {
		f.add("architecture_key must be an identifier like user_id")
	}
	timezone := "UTC"
	if req.Timezone != nil {
		timezone = *req.Timezone
		validateTimezone(&f, timezone)
	}
	if err := f.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	secret, prefix, hash, err := newAPIKey()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	tenant, key, err := s.store.CreateTenant(r.Context(), req.Name, req.ArchitectureKey, timezone, "default", prefix, hash)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, createTenantResponse{Tenant: tenant, APIKey: newKeyBody{APIKey: key, Secret: secret}})
}

// ---- Architecture ----

func (s *Server) getArchitecture(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]*string{"architecture_key": tenantFrom(r.Context()).ArchitectureKey})
}

type setArchitectureRequest struct {
	Key string `json:"key"`
}

func (s *Server) setArchitecture(w http.ResponseWriter, r *http.Request) {
	var req setArchitectureRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if !archKeyPattern.MatchString(req.Key) {
		s.writeError(w, r, apperr.New(http.StatusBadRequest, "validation_failed",
			"key must be an identifier like user_id or wallet_address"))
		return
	}
	tenant, err := s.store.SetArchitectureKey(r.Context(), tenantFrom(r.Context()).ID, req.Key)
	if errors.Is(err, store.ErrArchitectureLocked) {
		err = apperr.Conflict("architecture_locked",
			"the architecture key can't be changed after entity profiles have been created")
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]*string{"architecture_key": tenant.ArchitectureKey})
}

// ---- Settings ----

type settingsResponse struct {
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	t := tenantFrom(r.Context())
	writeJSON(w, http.StatusOK, settingsResponse{Name: t.Name, Timezone: t.Timezone})
}

type settingsRequest struct {
	Timezone *string `json:"timezone"`
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	t := tenantFrom(r.Context())
	if req.Timezone != nil {
		var f fieldErrors
		validateTimezone(&f, *req.Timezone)
		if err := f.err(); err != nil {
			s.writeError(w, r, err)
			return
		}
		var err error
		if t, err = s.store.SetTimezone(r.Context(), t.ID, *req.Timezone); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, settingsResponse{Name: t.Name, Timezone: t.Timezone})
}

// ---- API keys ----

type createAPIKeyRequest struct {
	Name string `json:"name"`
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var req createAPIKeyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var f fieldErrors
	f.text("name", req.Name, false, maxNameLen)
	if err := f.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	secret, prefix, hash, err := newAPIKey()
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	key, err := s.store.CreateAPIKey(r.Context(), tenantFrom(r.Context()).ID, req.Name, prefix, hash)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, newKeyBody{APIKey: key, Secret: secret})
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListAPIKeys(r.Context(), tenantFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": keys})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "key_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	key, err := s.store.RevokeAPIKey(r.Context(), tenantFrom(r.Context()).ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		err = apperr.NotFound("api_key_not_found", "API key not found")
	case errors.Is(err, store.ErrLastActiveAPIKey):
		err = apperr.Conflict("last_active_api_key", "create another API key before revoking the last active one")
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, key)
}

// ---- Tiers ----

type tierRequest struct {
	Slug        *string `json:"slug"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	IsActive    *bool   `json:"is_active"`
}

func (req tierRequest) apply(t *store.Tier) {
	set(&t.Slug, req.Slug)
	set(&t.Name, req.Name)
	set(&t.Description, req.Description)
	set(&t.IsActive, req.IsActive)
}

func (s *Server) createTier(w http.ResponseWriter, r *http.Request) {
	var req tierRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	t := store.Tier{TenantID: tenantFrom(r.Context()).ID, IsActive: true}
	req.apply(&t)
	if err := validateTier(t); err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := s.store.CreateTier(r.Context(), t)
	if err != nil {
		s.writeError(w, r, tierErr(err))
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTiers(w http.ResponseWriter, r *http.Request) {
	tiers, err := s.store.ListTiers(r.Context(), tenantFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": tiers})
}

func (s *Server) getTier(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "tier_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := s.store.GetTier(r.Context(), tenantFrom(r.Context()).ID, id)
	if err != nil {
		s.writeError(w, r, tierErr(err))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) updateTier(w http.ResponseWriter, r *http.Request) {
	var req tierRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.patchTier(w, r, req)
}

type statusRequest struct {
	IsActive *bool `json:"is_active"`
}

func decodeStatus(w http.ResponseWriter, r *http.Request) (bool, error) {
	var req statusRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return false, err
	}
	if req.IsActive == nil {
		return false, apperr.New(http.StatusBadRequest, "validation_failed", "is_active is required")
	}
	return *req.IsActive, nil
}

func (s *Server) setTierStatus(w http.ResponseWriter, r *http.Request) {
	active, err := decodeStatus(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.patchTier(w, r, tierRequest{IsActive: &active})
}

func (s *Server) patchTier(w http.ResponseWriter, r *http.Request, req tierRequest) {
	id, err := pathUUID(r, "tier_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	t, err := s.store.UpdateTier(r.Context(), tenantFrom(r.Context()).ID, id, func(t *store.Tier) error {
		req.apply(t)
		return validateTier(*t)
	})
	if err != nil {
		s.writeError(w, r, tierErr(err))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTier(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "tier_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.DeleteTier(r.Context(), tenantFrom(r.Context()).ID, id); err != nil {
		s.writeError(w, r, tierErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func tierErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("tier_not_found", "tier not found")
	case errors.Is(err, store.ErrConflict):
		return apperr.Conflict("tier_slug_taken", "a tier with that slug already exists")
	}
	return err
}

// ---- Rules ----

type ruleRequest struct {
	Slug        *string `json:"slug"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
	// PointDelta is shorthand for a single fixed effect on the primary point type
	// (negative to debit). It replaces the rule's effects, as Effects does.
	PointDelta             *int64          `json:"point_delta"`
	Effects                *[]store.Effect `json:"effects"`
	OrderDelta             *int            `json:"order_delta"`
	CooldownSeconds        *int            `json:"cooldown_seconds"`
	RateLimitMax           *int            `json:"rate_limit_max"`
	RateLimitWindowSeconds *int            `json:"rate_limit_window_seconds"`
	IsActive               *bool           `json:"is_active"`
}

// check rejects request-level mistakes that apply can't express.
func (req ruleRequest) check() error {
	var f fieldErrors
	if req.PointDelta != nil && req.Effects != nil {
		f.add("give either point_delta or effects, not both")
	}
	if req.PointDelta != nil && *req.PointDelta == 0 {
		f.add("point_delta must be a non-zero integer")
	}
	return f.err()
}

func (req ruleRequest) apply(r *store.Rule) {
	set(&r.Slug, req.Slug)
	set(&r.Name, req.Name)
	set(&r.Description, req.Description)
	if req.PointDelta != nil {
		e := store.Effect{Kind: store.EffectFixed, Direction: store.DirectionCredit, Amount: *req.PointDelta}
		if e.Amount < 0 {
			e.Direction, e.Amount = store.DirectionDebit, -e.Amount
		}
		r.Effects = []store.Effect{e}
	}
	if req.Effects != nil {
		r.Effects = make([]store.Effect, len(*req.Effects))
		for i, e := range *req.Effects {
			if e.Direction == "" {
				e.Direction = store.DirectionCredit
			}
			r.Effects[i] = e
		}
	}
	set(&r.OrderDelta, req.OrderDelta)
	set(&r.CooldownSeconds, req.CooldownSeconds)
	set(&r.RateLimitMax, req.RateLimitMax)
	set(&r.RateLimitWindowSeconds, req.RateLimitWindowSeconds)
	set(&r.IsActive, req.IsActive)
}

func (s *Server) createRule(w http.ResponseWriter, r *http.Request) {
	tierID, err := pathUUID(r, "tier_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req ruleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := req.check(); err != nil {
		s.writeError(w, r, err)
		return
	}
	rule := store.Rule{TenantID: tenantFrom(r.Context()).ID, TierID: tierID, IsActive: true}
	req.apply(&rule)
	if err := validateRule(rule); err != nil {
		s.writeError(w, r, err)
		return
	}
	rule, err = s.store.CreateRule(r.Context(), rule)
	if errors.Is(err, store.ErrNotFound) {
		err = apperr.NotFound("tier_not_found", "tier not found")
	}
	if err != nil {
		s.writeError(w, r, ruleErr(err))
		return
	}
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) listRules(w http.ResponseWriter, r *http.Request) {
	tierID, err := pathUUID(r, "tier_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rules, err := s.store.ListRules(r.Context(), tenantFrom(r.Context()).ID, tierID)
	if err != nil {
		s.writeError(w, r, tierErr(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rules})
}

func (s *Server) getRule(w http.ResponseWriter, r *http.Request) {
	tierID, ruleID, err := ruleIDs(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rule, err := s.store.GetRule(r.Context(), tenantFrom(r.Context()).ID, tierID, ruleID)
	if err != nil {
		s.writeError(w, r, ruleErr(err))
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) updateRule(w http.ResponseWriter, r *http.Request) {
	var req ruleRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := req.check(); err != nil {
		s.writeError(w, r, err)
		return
	}
	s.patchRule(w, r, req)
}

func (s *Server) setRuleStatus(w http.ResponseWriter, r *http.Request) {
	active, err := decodeStatus(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.patchRule(w, r, ruleRequest{IsActive: &active})
}

func (s *Server) patchRule(w http.ResponseWriter, r *http.Request, req ruleRequest) {
	tierID, ruleID, err := ruleIDs(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rule, err := s.store.UpdateRule(r.Context(), tenantFrom(r.Context()).ID, tierID, ruleID, func(rule *store.Rule) error {
		req.apply(rule)
		return validateRule(*rule)
	})
	if err != nil {
		s.writeError(w, r, ruleErr(err))
		return
	}
	writeJSON(w, http.StatusOK, rule)
}

func (s *Server) deleteRule(w http.ResponseWriter, r *http.Request) {
	tierID, ruleID, err := ruleIDs(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.DeleteRule(r.Context(), tenantFrom(r.Context()).ID, tierID, ruleID); err != nil {
		s.writeError(w, r, ruleErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func ruleIDs(r *http.Request) (tierID, ruleID uuid.UUID, err error) {
	if tierID, err = pathUUID(r, "tier_id"); err != nil {
		return
	}
	ruleID, err = pathUUID(r, "rule_id")
	return
}

func ruleErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("rule_not_found", "rule not found")
	case errors.Is(err, store.ErrConflict):
		return apperr.Conflict("rule_slug_taken", "a rule with that slug already exists in this tier")
	case errors.Is(err, store.ErrDuplicateEffect):
		return apperr.New(http.StatusBadRequest, "validation_failed", "%v", err)
	}
	return pointTypeErr(err)
}

// pointTypeErr maps errors naming a point type that a rule, level or adjustment referred to.
func pointTypeErr(err error) error {
	var pe *store.PointTypeError
	if !errors.As(err, &pe) {
		return err
	}
	switch {
	case errors.Is(err, store.ErrPointTypeNotFound):
		return apperr.NotFound("point_type_not_found", "point type %q does not exist", pe.Slug)
	case errors.Is(err, store.ErrPointTypeNotSpendable):
		return apperr.New(http.StatusUnprocessableEntity, "point_type_not_spendable",
			"point type %q is not spendable, so rules can't debit it", pe.Slug)
	}
	return err
}

// ---- Adjustments (compensating transactions) ----

type adjustmentRequest struct {
	EntityID string `json:"entity_id"`
	// PointType is the slug Points applies to; empty means the primary type.
	PointType      string `json:"point_type"`
	Points         int64  `json:"points"`
	Spend          int64  `json:"spend"`
	Orders         int64  `json:"orders"`
	Reason         string `json:"reason"`
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) createAdjustment(w http.ResponseWriter, r *http.Request) {
	var req adjustmentRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	var f fieldErrors
	validateEntityID(&f, "entity_id", req.EntityID)
	if req.Points == 0 && req.Spend == 0 && req.Orders == 0 {
		f.add("points, spend or orders must be a non-zero integer")
	}
	if req.PointType != "" && !slugPattern.MatchString(req.PointType) {
		f.add("point_type must be a point type slug")
	}
	if req.Points < -maxPoints || req.Points > maxPoints {
		f.add("points must be between -10^15 and 10^15")
	}
	if req.Orders < -maxOrders || req.Orders > maxOrders {
		f.add("orders must be between -10^9 and 10^9")
	}
	validateSpend(&f, req.Spend)
	f.text("reason", req.Reason, true, maxReasonLen)
	validateIdempotencyKey(&f, req.IdempotencyKey)
	if err := f.err(); err != nil {
		s.writeError(w, r, err)
		return
	}
	res, err := s.engine.Adjust(r.Context(), tenantFrom(r.Context()), engine.AdjustRequest{
		EntityID: req.EntityID, PointType: req.PointType, Points: req.Points, Spend: req.Spend, Orders: req.Orders,
		Reason: req.Reason, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeResult(w, res)
}

// ---- helpers ----

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		return uuid.Nil, apperr.BadRequest("%s must be a UUID", name)
	}
	return id, nil
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}
