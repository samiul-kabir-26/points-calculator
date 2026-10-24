package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const profileSelect = `
	SELECT p.entity_id, p.current_tier_id, t.slug, p.lifetime_spend, p.qualifying_spend, p.lifetime_orders,
		p.qualifying_orders, p.current_level_id, l.slug, p.level_since, p.below_level_since, p.created_at, p.updated_at,
		COALESCE((
			SELECT b.balance FROM entity_balances b
			JOIN point_types pt ON pt.tenant_id = b.tenant_id AND pt.id = b.point_type_id AND pt.is_primary
			WHERE b.tenant_id = p.tenant_id AND b.entity_id = p.entity_id), 0),
		COALESCE((
			SELECT jsonb_object_agg(pt.slug, jsonb_build_object('balance', b.balance,
				'lifetime_earned', b.lifetime_earned, 'qualifying_earned', b.qualifying_earned))
			FROM entity_balances b JOIN point_types pt ON pt.tenant_id = b.tenant_id AND pt.id = b.point_type_id
			WHERE b.tenant_id = p.tenant_id AND b.entity_id = p.entity_id), '{}')
	FROM entity_profiles p
	LEFT JOIN tiers t ON t.id = p.current_tier_id
	LEFT JOIN levels l ON l.id = p.current_level_id`

func scanProfile(row pgx.Row) (Profile, error) {
	var (
		p        Profile
		balances []byte
	)
	err := row.Scan(&p.EntityID, &p.TierID, &p.TierSlug, &p.LifetimeSpend, &p.QualifyingSpend, &p.LifetimeOrders,
		&p.QualifyingOrders, &p.LevelID, &p.LevelSlug, &p.LevelSince, &p.BelowLevelSince, &p.CreatedAt, &p.UpdatedAt,
		&p.Balance, &balances)
	if err != nil {
		return p, translate(err)
	}
	p.Balances = map[string]Balance{}
	return p, json.Unmarshal(balances, &p.Balances)
}

func (s *Store) GetProfile(ctx context.Context, tenantID uuid.UUID, entityID string) (Profile, error) {
	return scanProfile(s.pool.QueryRow(ctx,
		profileSelect+` WHERE p.tenant_id = $1 AND p.entity_id = $2`, tenantID, entityID))
}

type ProfileFilter struct {
	TierSlug  *string
	LevelSlug *string
	// After is a keyset cursor: only profiles with entity_id greater than it are returned.
	After *string
	Limit int
}

// ListProfiles returns profiles ordered by entity_id.
func (s *Store) ListProfiles(ctx context.Context, tenantID uuid.UUID, f ProfileFilter) ([]Profile, error) {
	where := []string{"p.tenant_id = $1"}
	args := []any{tenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.TierSlug != nil {
		add("t.slug = $%d", *f.TierSlug)
	}
	if f.LevelSlug != nil {
		add("l.slug = $%d", *f.LevelSlug)
	}
	if f.After != nil {
		add("p.entity_id > $%d", *f.After)
	}
	args = append(args, f.Limit)
	q := fmt.Sprintf("%s WHERE %s ORDER BY p.entity_id LIMIT $%d", profileSelect, strings.Join(where, " AND "), len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanProfile)
}
