// Package engine executes point transactions: it validates the rule, enforces
// cooldown/throttle limits, handles idempotency, and atomically applies the rule's effects.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/limiter"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

// idempotencyLockTTL bounds how long a crashed request can block retries of the same key.
const idempotencyLockTTL = 30 * time.Second

type Engine struct {
	store   *store.Store
	limiter *limiter.Limiter
	log     *slog.Logger
}

func New(s *store.Store, l *limiter.Limiter, log *slog.Logger) *Engine {
	return &Engine{store: s, limiter: l, log: log}
}

type ExecuteRequest struct {
	EntityID string
	// Tier may be empty, in which case the tier linked to the entity's level is used.
	Tier  string
	Rule  string
	Spend int64
	// Amount is the caller-supplied amount for rules with an input effect; nil when absent.
	Amount         *int64
	IdempotencyKey string
}

type AdjustRequest struct {
	EntityID string
	// PointType is the slug of the type Points applies to; empty means the primary type.
	PointType      string
	Points         int64
	Spend          int64
	Orders         int64
	Reason         string
	IdempotencyKey string
}

type Result struct {
	Entry store.LedgerEntry
	// LevelChange is set when this transaction moved the entity to another level.
	// It isn't reconstructed for replays.
	LevelChange *store.LevelChange
	// Streak is the entity's streak length after this claim, for streak rules. It isn't
	// reconstructed for replays.
	Streak *int
	// Replayed is true when the response is the stored result of an earlier
	// request with the same idempotency key.
	Replayed bool
}

// Execute triggers a rule for an entity.
func (e *Engine) Execute(ctx context.Context, tenant store.Tenant, req ExecuteRequest) (Result, error) {
	if tenant.ArchitectureKey == nil {
		return Result{}, apperr.Conflict("architecture_not_configured",
			"set the architecture key with POST /admin/architecture before executing rules")
	}
	matches := func(prev store.LedgerEntry) bool {
		// A tier resolved from the entity's level may differ on retry, so it isn't compared.
		return prev.EntryType == store.EntryTypeRule && prev.EntityID == req.EntityID &&
			(req.Tier == "" || deref(prev.TierSlug) == req.Tier) && deref(prev.RuleSlug) == req.Rule &&
			prev.Spend == req.Spend && sameAmount(prev.InputAmount, req.Amount)
	}
	return e.withIdempotency(ctx, tenant.ID, req.IdempotencyKey, matches, func() (Result, error) {
		return e.execute(ctx, tenant, req)
	})
}

func (e *Engine) execute(ctx context.Context, tenant store.Tenant, req ExecuteRequest) (Result, error) {
	tier := req.Tier
	if tier == "" {
		slug, err := e.store.GetLevelTierSlug(ctx, tenant.ID, req.EntityID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return Result{}, apperr.New(http.StatusUnprocessableEntity, "tier_unresolved",
				"tier is required: entity %q has no level with a linked tier", req.EntityID)
		case err != nil:
			return Result{}, err
		}
		tier = slug
	}
	rule, err := e.store.GetExecutableRule(ctx, tenant.ID, tier, req.Rule)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Result{}, apperr.NotFound("tier_not_found", "tier %q does not exist", tier)
	case errors.Is(err, store.ErrRuleNotFoundInTier):
		return Result{}, apperr.NotFound("rule_not_found", "rule %q does not exist in tier %q", req.Rule, tier)
	case err != nil:
		return Result{}, err
	}
	if !rule.TierIsActive {
		return Result{}, apperr.New(http.StatusForbidden, "tier_inactive", "tier %q is inactive", tier)
	}
	if !rule.IsActive {
		return Result{}, apperr.New(http.StatusForbidden, "rule_inactive", "rule %q is disabled", req.Rule)
	}
	if err := checkAmount(rule.Rule, req.Amount); err != nil {
		return Result{}, err
	}

	decision, reservation, err := e.limiter.Reserve(ctx, tenant.ID.String(), rule.ID.String(), req.EntityID,
		uuid.NewString(), limiter.Limits{
			Cooldown: time.Duration(rule.CooldownSeconds) * time.Second,
			MaxHits:  rule.RateLimitMax,
			Window:   time.Duration(rule.RateLimitWindowSeconds) * time.Second,
		})
	if err != nil {
		// Fail closed: without Redis the limits can't be enforced.
		e.log.ErrorContext(ctx, "limiter unavailable", "err", err)
		return Result{}, apperr.New(http.StatusServiceUnavailable, "limiter_unavailable",
			"rate limiting backend is unavailable, retry later")
	}
	switch decision.Outcome {
	case limiter.CooldownActive:
		ae := apperr.New(http.StatusTooManyRequests, "cooldown_active",
			"rule %q is cooling down for this entity", req.Rule)
		ae.RetryAfter = decision.RetryAfter
		return Result{}, ae
	case limiter.RateLimited:
		ae := apperr.New(http.StatusTooManyRequests, "rate_limit_exceeded",
			"rule %q may be triggered at most %d times per %ds", req.Rule, rule.RateLimitMax, rule.RateLimitWindowSeconds)
		ae.RetryAfter = decision.RetryAfter
		return Result{}, ae
	}

	applied, err := e.store.AppendEntry(ctx, store.NewEntry{
		TenantID:       tenant.ID,
		EntityID:       req.EntityID,
		EntryType:      store.EntryTypeRule,
		TierID:         &rule.TierID,
		TierSlug:       &rule.TierSlug,
		RuleID:         &rule.ID,
		RuleSlug:       &rule.Slug,
		Effects:        rule.Effects,
		Spend:          req.Spend,
		Orders:         int64(rule.OrderDelta),
		InputAmount:    req.Amount,
		IdempotencyKey: optional(req.IdempotencyKey),
		Timezone:       tenant.Timezone,
		Levels:         tenant.LevelPolicy,
	})
	if err != nil {
		// The trigger didn't happen, so it must not count against the entity's limits.
		if rerr := e.limiter.Release(context.WithoutCancel(ctx), reservation); rerr != nil {
			e.log.WarnContext(ctx, "failed to release limiter reservation", "err", rerr)
		}
		return Result{}, mapAppendErr(err)
	}
	return Result{Entry: applied.Entry, LevelChange: applied.LevelChange, Streak: applied.Streak}, nil
}

// checkAmount validates the request's amount against the rule's input effect, if any.
func checkAmount(rule store.Rule, amount *int64) error {
	var input *store.Effect
	for i := range rule.Effects {
		if rule.Effects[i].Kind == store.EffectInput {
			input = &rule.Effects[i]
		}
	}
	switch {
	case input == nil && amount != nil:
		return apperr.New(http.StatusUnprocessableEntity, "amount_not_accepted",
			"rule %q has no input effect, so it doesn't take an amount", rule.Slug)
	case input == nil:
		return nil
	case amount == nil || *amount <= 0:
		return apperr.New(http.StatusUnprocessableEntity, "amount_required",
			"rule %q needs a positive amount of %s", rule.Slug, input.PointType)
	case *amount < input.Min || (input.Max > 0 && *amount > input.Max):
		bound := "no maximum"
		if input.Max > 0 {
			bound = fmt.Sprintf("at most %d", input.Max)
		}
		return apperr.New(http.StatusUnprocessableEntity, "amount_out_of_range",
			"amount for rule %q must be at least %d and %s", rule.Slug, max(input.Min, 1), bound)
	}
	return nil
}

// Adjust records a compensating transaction. The ledger is append-only, so this
// is the only way to correct a balance.
func (e *Engine) Adjust(ctx context.Context, tenant store.Tenant, req AdjustRequest) (Result, error) {
	matches := func(prev store.LedgerEntry) bool {
		if prev.EntryType != store.EntryTypeAdjustment || prev.EntityID != req.EntityID ||
			prev.Spend != req.Spend || prev.Orders != req.Orders {
			return false
		}
		if req.Points == 0 {
			return len(prev.Lines) == 0
		}
		return len(prev.Lines) == 1 && prev.Lines[0].Delta == req.Points &&
			(req.PointType == "" || prev.Lines[0].PointType == req.PointType)
	}
	var effects []store.Effect
	if req.Points != 0 {
		ef := store.Effect{PointType: req.PointType, Kind: store.EffectFixed, Direction: store.DirectionCredit, Amount: req.Points}
		if req.Points < 0 {
			ef.Direction, ef.Amount = store.DirectionDebit, -req.Points
		}
		effects = append(effects, ef)
	}
	return e.withIdempotency(ctx, tenant.ID, req.IdempotencyKey, matches, func() (Result, error) {
		applied, err := e.store.AppendEntry(ctx, store.NewEntry{
			TenantID:       tenant.ID,
			EntityID:       req.EntityID,
			EntryType:      store.EntryTypeAdjustment,
			Effects:        effects,
			Spend:          req.Spend,
			Orders:         req.Orders,
			IdempotencyKey: optional(req.IdempotencyKey),
			Reason:         &req.Reason,
			Timezone:       tenant.Timezone,
			Levels:         tenant.LevelPolicy,
		})
		return Result{Entry: applied.Entry, LevelChange: applied.LevelChange}, mapAppendErr(err)
	})
}

// withIdempotency runs apply at most once per idempotency key. Repeats of a
// completed request get the original ledger entry back; a key reused for a
// different request is rejected. The Redis lock stops concurrent duplicates from
// both consuming rate-limit slots, and the ledger's unique index is the final guard.
func (e *Engine) withIdempotency(ctx context.Context, tenantID uuid.UUID, key string,
	matches func(store.LedgerEntry) bool, apply func() (Result, error),
) (Result, error) {
	if key == "" {
		return apply()
	}

	replay := func() (Result, bool, error) {
		prev, err := e.store.GetEntryByIdempotencyKey(ctx, tenantID, key)
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, false, nil
		}
		if err != nil {
			return Result{}, false, err
		}
		if !matches(prev) {
			return Result{}, true, apperr.New(http.StatusUnprocessableEntity, "idempotency_key_reused",
				"idempotency key %q was already used for a different request", key)
		}
		return Result{Entry: prev, Replayed: true}, true, nil
	}

	lock, err := e.limiter.TryLock(ctx, "pc:idem:"+tenantID.String()+":"+key, uuid.NewString(), idempotencyLockTTL)
	if err != nil {
		e.log.ErrorContext(ctx, "idempotency lock unavailable", "err", err)
		return Result{}, apperr.New(http.StatusServiceUnavailable, "limiter_unavailable",
			"rate limiting backend is unavailable, retry later")
	}
	if lock == nil {
		if res, found, err := replay(); found || err != nil {
			return res, err
		}
		return Result{}, apperr.Conflict("idempotency_in_progress",
			"a request with idempotency key %q is still being processed", key)
	}
	defer func() {
		if err := e.limiter.Unlock(context.WithoutCancel(ctx), lock); err != nil {
			e.log.WarnContext(ctx, "failed to release idempotency lock", "err", err)
		}
	}()

	if res, found, err := replay(); found || err != nil {
		return res, err
	}
	res, err := apply()
	if errors.Is(err, store.ErrDuplicateIdempotency) {
		// Lost a race the lock didn't cover (e.g. the lock expired); the winner's entry is authoritative.
		if res, found, rerr := replay(); found || rerr != nil {
			return res, rerr
		}
	}
	return res, err
}

func mapAppendErr(err error) error {
	var (
		be *store.BalanceError
		le *store.LimitError
		se *store.StreakError
		pe *store.PointTypeError
	)
	switch {
	case err == nil:
		return nil
	case errors.As(err, &be):
		return apperr.New(http.StatusUnprocessableEntity, "insufficient_balance",
			"the entity's %s balance is too low for this deduction", be.PointType)
	case errors.As(err, &le):
		return apperr.New(http.StatusUnprocessableEntity, "point_limit_exceeded",
			"this would exceed the %s %s limit of %d per %s (%d left)",
			le.PointType, le.Limit.Direction, le.Limit.Max, le.Limit.Period, le.Remaining)
	case errors.As(err, &se):
		ae := apperr.New(http.StatusTooManyRequests, "streak_already_claimed",
			"this streak has already been claimed today")
		ae.RetryAfter = se.RetryAfter
		return ae
	case errors.As(err, &pe) && errors.Is(err, store.ErrPointTypeNotFound):
		return apperr.NotFound("point_type_not_found", "point type %q does not exist", pe.Slug)
	case errors.Is(err, store.ErrOrdersBelowZero):
		return apperr.New(http.StatusUnprocessableEntity, "orders_below_zero",
			"the order correction exceeds the entity's recorded orders")
	case errors.Is(err, store.ErrSpendBelowZero):
		return apperr.New(http.StatusUnprocessableEntity, "spend_below_zero",
			"the refund exceeds the entity's recorded spend")
	case errors.Is(err, store.ErrBalanceOverflow):
		return apperr.New(http.StatusUnprocessableEntity, "balance_overflow",
			"the resulting balance is out of range")
	}
	return err
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func sameAmount(a, b *int64) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
