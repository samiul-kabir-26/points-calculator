package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const ruleColumns = `id, tenant_id, tier_id, slug, name, description, order_delta, cooldown_seconds,
	rate_limit_max, rate_limit_window_seconds, is_active, created_at, updated_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	err := row.Scan(&r.ID, &r.TenantID, &r.TierID, &r.Slug, &r.Name, &r.Description, &r.OrderDelta,
		&r.CooldownSeconds, &r.RateLimitMax, &r.RateLimitWindowSeconds, &r.IsActive, &r.CreatedAt, &r.UpdatedAt)
	r.Effects = []Effect{}
	return r, translate(err)
}

// loadEffects attaches each rule's effects, in their saved order.
func loadEffects(ctx context.Context, q querier, tenantID uuid.UUID, rules []*Rule) error {
	if len(rules) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(rules))
	byID := make(map[uuid.UUID]*Rule, len(rules))
	for i, r := range rules {
		ids[i] = r.ID
		byID[r.ID] = r
	}
	rows, err := q.Query(ctx, `
		SELECT e.rule_id, e.point_type_id, pt.slug, e.kind, e.direction, e.amount, e.rate_points, e.rate_per,
			e.min_amount, e.max_amount, e.steps, e.expiry_mode, e.expiry_value
		FROM rule_effects e JOIN point_types pt ON pt.tenant_id = e.tenant_id AND pt.id = e.point_type_id
		WHERE e.tenant_id = $1 AND e.rule_id = ANY($2)
		ORDER BY e.position`, tenantID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			ruleID uuid.UUID
			e      Effect
		)
		if err := rows.Scan(&ruleID, &e.PointTypeID, &e.PointType, &e.Kind, &e.Direction, &e.Amount, &e.RatePoints,
			&e.RatePer, &e.Min, &e.Max, &e.Steps, &e.ExpiryMode, &e.ExpiryValue); err != nil {
			return err
		}
		if len(e.Steps) == 0 {
			e.Steps = nil
		}
		byID[ruleID].Effects = append(byID[ruleID].Effects, e)
	}
	return rows.Err()
}

// saveEffects replaces a rule's effects, resolving point type slugs ("" is the primary type).
// Debits on non-spendable types are refused.
func saveEffects(ctx context.Context, tx pgx.Tx, tenantID, ruleID uuid.UUID, effects []Effect) error {
	if _, err := tx.Exec(ctx, `DELETE FROM rule_effects WHERE tenant_id = $1 AND rule_id = $2`, tenantID, ruleID); err != nil {
		return err
	}
	if len(effects) == 0 {
		return nil
	}
	types, err := loadPointTypeSet(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	for i, e := range effects {
		pt, err := types.resolve(e.PointType)
		if err != nil {
			return err
		}
		if e.Direction == DirectionDebit && !pt.Spendable {
			return &PointTypeError{Slug: pt.Slug, Err: ErrPointTypeNotSpendable}
		}
		steps := e.Steps
		if steps == nil {
			steps = []int64{}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO rule_effects (rule_id, tenant_id, point_type_id, position, kind, direction, amount,
				rate_points, rate_per, min_amount, max_amount, steps, expiry_mode, expiry_value)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			ruleID, tenantID, pt.ID, i, e.Kind, e.Direction, e.Amount, e.RatePoints, e.RatePer, e.Min, e.Max,
			steps, e.ExpiryMode, e.ExpiryValue); err != nil {
			if pe, ok := pgErr(err); ok && pe.Code == pgUniqueViolation {
				return ErrDuplicateEffect
			}
			return err
		}
	}
	return nil
}

// CreateRule inserts a rule with its effects. A tier that doesn't exist for this tenant yields
// ErrNotFound (enforced by the composite (tenant_id, tier_id) foreign key).
func (s *Store) CreateRule(ctx context.Context, r Rule) (Rule, error) {
	var out Rule
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanRule(tx.QueryRow(ctx, `
			INSERT INTO rules (tenant_id, tier_id, slug, name, description, order_delta, cooldown_seconds,
				rate_limit_max, rate_limit_window_seconds, is_active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+ruleColumns,
			r.TenantID, r.TierID, r.Slug, r.Name, r.Description, r.OrderDelta, r.CooldownSeconds,
			r.RateLimitMax, r.RateLimitWindowSeconds, r.IsActive))
		if err != nil {
			return err
		}
		if err := saveEffects(ctx, tx, r.TenantID, out.ID, r.Effects); err != nil {
			return err
		}
		return loadEffects(ctx, tx, r.TenantID, []*Rule{&out})
	})
	return out, err
}

func (s *Store) ListRules(ctx context.Context, tenantID, tierID uuid.UUID) ([]Rule, error) {
	if _, err := s.GetTier(ctx, tenantID, tierID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+ruleColumns+` FROM rules WHERE tenant_id = $1 AND tier_id = $2 ORDER BY created_at, id`,
		tenantID, tierID)
	if err != nil {
		return nil, err
	}
	rules, err := collect(rows, scanRule)
	if err != nil {
		return nil, err
	}
	ptrs := make([]*Rule, len(rules))
	for i := range rules {
		ptrs[i] = &rules[i]
	}
	return rules, loadEffects(ctx, s.pool, tenantID, ptrs)
}

func (s *Store) GetRule(ctx context.Context, tenantID, tierID, ruleID uuid.UUID) (Rule, error) {
	r, err := scanRule(s.pool.QueryRow(ctx,
		`SELECT `+ruleColumns+` FROM rules WHERE tenant_id = $1 AND tier_id = $2 AND id = $3`,
		tenantID, tierID, ruleID))
	if err != nil {
		return r, err
	}
	return r, loadEffects(ctx, s.pool, tenantID, []*Rule{&r})
}

// UpdateRule loads the rule and its effects under a row lock, lets mutate change them, and
// saves the result.
func (s *Store) UpdateRule(ctx context.Context, tenantID, tierID, ruleID uuid.UUID, mutate func(*Rule) error) (Rule, error) {
	var r Rule
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		r, err = scanRule(tx.QueryRow(ctx,
			`SELECT `+ruleColumns+` FROM rules WHERE tenant_id = $1 AND tier_id = $2 AND id = $3 FOR UPDATE`,
			tenantID, tierID, ruleID))
		if err != nil {
			return err
		}
		if err := loadEffects(ctx, tx, tenantID, []*Rule{&r}); err != nil {
			return err
		}
		if err := mutate(&r); err != nil {
			return err
		}
		effects := r.Effects
		r, err = scanRule(tx.QueryRow(ctx, `
			UPDATE rules SET slug = $4, name = $5, description = $6, order_delta = $7, cooldown_seconds = $8,
				rate_limit_max = $9, rate_limit_window_seconds = $10, is_active = $11, updated_at = now()
			WHERE tenant_id = $1 AND tier_id = $2 AND id = $3
			RETURNING `+ruleColumns,
			tenantID, tierID, ruleID, r.Slug, r.Name, r.Description, r.OrderDelta, r.CooldownSeconds,
			r.RateLimitMax, r.RateLimitWindowSeconds, r.IsActive))
		if err != nil {
			return err
		}
		if err := saveEffects(ctx, tx, tenantID, ruleID, effects); err != nil {
			return err
		}
		return loadEffects(ctx, tx, tenantID, []*Rule{&r})
	})
	return r, err
}

func (s *Store) DeleteRule(ctx context.Context, tenantID, tierID, ruleID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM rules WHERE tenant_id = $1 AND tier_id = $2 AND id = $3`, tenantID, tierID, ruleID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetExecutableRule resolves a rule and its effects by tier and rule slug. It returns
// ErrNotFound when the tier doesn't exist and ErrRuleNotFoundInTier when the tier exists but
// the rule doesn't.
func (s *Store) GetExecutableRule(ctx context.Context, tenantID uuid.UUID, tierSlug, ruleSlug string) (ExecutableRule, error) {
	var er ExecutableRule
	r := &er.Rule
	err := s.pool.QueryRow(ctx, `
		SELECT r.id, r.tenant_id, r.tier_id, r.slug, r.name, r.description, r.order_delta, r.cooldown_seconds,
			r.rate_limit_max, r.rate_limit_window_seconds, r.is_active, r.created_at, r.updated_at,
			t.slug, t.is_active
		FROM rules r JOIN tiers t ON t.tenant_id = r.tenant_id AND t.id = r.tier_id
		WHERE r.tenant_id = $1 AND t.slug = $2 AND r.slug = $3`,
		tenantID, tierSlug, ruleSlug,
	).Scan(&r.ID, &r.TenantID, &r.TierID, &r.Slug, &r.Name, &r.Description, &r.OrderDelta,
		&r.CooldownSeconds, &r.RateLimitMax, &r.RateLimitWindowSeconds, &r.IsActive, &r.CreatedAt, &r.UpdatedAt,
		&er.TierSlug, &er.TierIsActive)
	if err == nil {
		r.Effects = []Effect{}
		return er, loadEffects(ctx, s.pool, tenantID, []*Rule{r})
	}
	if err = translate(err); err != ErrNotFound {
		return er, err
	}
	// Distinguish an unknown tier from an unknown rule; only paid on the error path.
	var tierExists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tiers WHERE tenant_id = $1 AND slug = $2)`, tenantID, tierSlug,
	).Scan(&tierExists); err != nil {
		return er, err
	}
	if tierExists {
		return er, ErrRuleNotFoundInTier
	}
	return er, ErrNotFound
}
