package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const levelColumns = "id, tenant_id, slug, name, description, rank, min_spend, min_orders, tier_id, is_active, created_at, updated_at"

func scanLevel(row pgx.Row) (Level, error) {
	var l Level
	err := row.Scan(&l.ID, &l.TenantID, &l.Slug, &l.Name, &l.Description, &l.Rank, &l.MinSpend, &l.MinOrders,
		&l.TierID, &l.IsActive, &l.CreatedAt, &l.UpdatedAt)
	l.MinPoints = map[string]int64{}
	return l, translateLevel(err)
}

// translateLevel tells the two unique constraints and the tier foreign key apart.
func translateLevel(err error) error {
	if pe, ok := pgErr(err); ok {
		switch {
		case pe.Code == pgUniqueViolation && pe.ConstraintName == "levels_tenant_rank_key":
			return ErrLevelRankTaken
		case pe.Code == pgForeignKeyViolation && pe.ConstraintName == "levels_tier_fkey":
			return ErrLevelTierNotFound
		}
	}
	return translate(err)
}

// loadRequirements attaches each level's per-point-type minimums, keyed by slug.
func loadRequirements(ctx context.Context, q querier, tenantID uuid.UUID, levels []*Level) error {
	if len(levels) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(levels))
	byID := make(map[uuid.UUID]*Level, len(levels))
	for i, l := range levels {
		ids[i] = l.ID
		byID[l.ID] = l
	}
	rows, err := q.Query(ctx, `
		SELECT r.level_id, pt.slug, r.min_points
		FROM level_requirements r JOIN point_types pt ON pt.tenant_id = r.tenant_id AND pt.id = r.point_type_id
		WHERE r.tenant_id = $1 AND r.level_id = ANY($2)`, tenantID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			levelID uuid.UUID
			slug    string
			min     int64
		)
		if err := rows.Scan(&levelID, &slug, &min); err != nil {
			return err
		}
		byID[levelID].MinPoints[slug] = min
	}
	return rows.Err()
}

// saveRequirements replaces a level's per-point-type minimums. Zero minimums are dropped.
func saveRequirements(ctx context.Context, tx pgx.Tx, tenantID, levelID uuid.UUID, minPoints map[string]int64) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM level_requirements WHERE tenant_id = $1 AND level_id = $2`, tenantID, levelID); err != nil {
		return err
	}
	if len(minPoints) == 0 {
		return nil
	}
	types, err := loadPointTypeSet(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	for slug, min := range minPoints {
		if min == 0 {
			continue
		}
		pt, err := types.resolve(slug)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO level_requirements (level_id, tenant_id, point_type_id, min_points) VALUES ($1, $2, $3, $4)`,
			levelID, tenantID, pt.ID, min); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreateLevel(ctx context.Context, l Level) (Level, error) {
	var out Level
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanLevel(tx.QueryRow(ctx, `
			INSERT INTO levels (tenant_id, slug, name, description, rank, min_spend, min_orders, tier_id, is_active)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+levelColumns,
			l.TenantID, l.Slug, l.Name, l.Description, l.Rank, l.MinSpend, l.MinOrders, l.TierID, l.IsActive))
		if err != nil {
			return err
		}
		if err := saveRequirements(ctx, tx, l.TenantID, out.ID, l.MinPoints); err != nil {
			return err
		}
		return loadRequirements(ctx, tx, l.TenantID, []*Level{&out})
	})
	return out, err
}

// ListLevels returns levels from the lowest rank to the highest.
func (s *Store) ListLevels(ctx context.Context, tenantID uuid.UUID) ([]Level, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+levelColumns+` FROM levels WHERE tenant_id = $1 ORDER BY rank`, tenantID)
	if err != nil {
		return nil, err
	}
	levels, err := collect(rows, scanLevel)
	if err != nil {
		return nil, err
	}
	ptrs := make([]*Level, len(levels))
	for i := range levels {
		ptrs[i] = &levels[i]
	}
	return levels, loadRequirements(ctx, s.pool, tenantID, ptrs)
}

func (s *Store) GetLevel(ctx context.Context, tenantID, levelID uuid.UUID) (Level, error) {
	l, err := scanLevel(s.pool.QueryRow(ctx,
		`SELECT `+levelColumns+` FROM levels WHERE tenant_id = $1 AND id = $2`, tenantID, levelID))
	if err != nil {
		return l, err
	}
	return l, loadRequirements(ctx, s.pool, tenantID, []*Level{&l})
}

// UpdateLevel loads the level under a row lock, lets mutate change it, and saves the result.
// Entities keep their current level until they are next evaluated.
func (s *Store) UpdateLevel(ctx context.Context, tenantID, levelID uuid.UUID, mutate func(*Level) error) (Level, error) {
	var l Level
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		l, err = scanLevel(tx.QueryRow(ctx,
			`SELECT `+levelColumns+` FROM levels WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, levelID))
		if err != nil {
			return err
		}
		if err := loadRequirements(ctx, tx, tenantID, []*Level{&l}); err != nil {
			return err
		}
		if err := mutate(&l); err != nil {
			return err
		}
		minPoints := l.MinPoints
		l, err = scanLevel(tx.QueryRow(ctx, `
			UPDATE levels SET slug = $3, name = $4, description = $5, rank = $6, min_spend = $7, min_orders = $8,
				tier_id = $9, is_active = $10, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+levelColumns,
			tenantID, levelID, l.Slug, l.Name, l.Description, l.Rank, l.MinSpend, l.MinOrders, l.TierID, l.IsActive))
		if err != nil {
			return err
		}
		if err := saveRequirements(ctx, tx, tenantID, levelID, minPoints); err != nil {
			return err
		}
		return loadRequirements(ctx, tx, tenantID, []*Level{&l})
	})
	return l, err
}

// DeleteLevel removes a level. Entities on it are left without a level until re-evaluated;
// level history is kept.
func (s *Store) DeleteLevel(ctx context.Context, tenantID, levelID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM levels WHERE tenant_id = $1 AND id = $2`, tenantID, levelID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateLevelPolicy loads the tenant's level policy under a row lock, lets mutate change it, and saves it.
func (s *Store) UpdateLevelPolicy(ctx context.Context, tenantID uuid.UUID, mutate func(*LevelPolicy) error) (LevelPolicy, error) {
	var t Tenant
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		t, err = scanTenant(tx.QueryRow(ctx,
			`SELECT `+tenantColumns+` FROM tenants WHERE id = $1 FOR UPDATE`, tenantID))
		if err != nil {
			return err
		}
		if err := mutate(&t.LevelPolicy); err != nil {
			return err
		}
		p := t.LevelPolicy
		t, err = scanTenant(tx.QueryRow(ctx, `
			UPDATE tenants SET level_basis = $2, level_window_days = $3, level_period_months = $4,
				level_downgrade = $5, level_grace_days = $6, level_min_tenure_days = $7, updated_at = now()
			WHERE id = $1
			RETURNING `+tenantColumns,
			tenantID, p.Basis, p.WindowDays, p.PeriodMonths, p.Downgrade, p.GraceDays, p.MinTenureDays))
		return err
	})
	return t.LevelPolicy, err
}

// GetLevelTierSlug returns the slug of the tier linked to the entity's current level.
// An entity without a level yet (including one with no profile) is treated as being on the
// highest level its qualifying spend, orders and points meet, so a base level with no
// requirements applies from the first transaction. It returns ErrNotFound when there is no
// such level or it has no tier.
func (s *Store) GetLevelTierSlug(ctx context.Context, tenantID uuid.UUID, entityID string) (string, error) {
	var slug *string
	err := s.pool.QueryRow(ctx, `
		WITH p AS (
			SELECT current_level_id, qualifying_spend, qualifying_orders FROM entity_profiles
			WHERE tenant_id = $1 AND entity_id = $2
		)
		SELECT t.slug
		FROM levels l
		LEFT JOIN tiers t ON t.tenant_id = l.tenant_id AND t.id = l.tier_id
		WHERE l.tenant_id = $1 AND l.is_active AND (
			l.id = (SELECT current_level_id FROM p)
			OR ((SELECT current_level_id FROM p) IS NULL
				AND l.min_spend <= COALESCE((SELECT qualifying_spend FROM p), 0)
				AND l.min_orders <= COALESCE((SELECT qualifying_orders FROM p), 0)
				AND NOT EXISTS (
					SELECT 1 FROM level_requirements r
					LEFT JOIN entity_balances b ON b.tenant_id = r.tenant_id AND b.entity_id = $2
						AND b.point_type_id = r.point_type_id
					WHERE r.tenant_id = $1 AND r.level_id = l.id AND r.min_points > COALESCE(b.qualifying_earned, 0))))
		ORDER BY l.rank DESC LIMIT 1`,
		tenantID, entityID).Scan(&slug)
	if err == nil && slug == nil {
		return "", ErrNotFound
	}
	return deref(slug), translate(err)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ladderLevel is a level as the evaluator sees it.
type ladderLevel struct {
	id        uuid.UUID
	slug      string
	rank      int
	active    bool
	minSpend  int64
	minOrders int64
	minPoints map[uuid.UUID]int64
}

// activity is what an entity did within a qualifying window.
type activity struct {
	spend, orders int64
	// points earned per point type: rule credits and adjustments, not redemptions or expiry.
	points map[uuid.UUID]int64
}

func (l *ladderLevel) metBy(a activity) bool {
	if a.spend < l.minSpend || a.orders < l.minOrders {
		return false
	}
	for id, min := range l.minPoints {
		if a.points[id] < min {
			return false
		}
	}
	return true
}

// evaluator moves entities between levels under one tenant's policy.
type evaluator struct {
	tenantID uuid.UUID
	policy   LevelPolicy
	loc      *time.Location
	types    pointTypeSet
	levels   map[uuid.UUID]*ladderLevel
	// active holds the active levels, highest rank first.
	active []*ladderLevel
}

func newEvaluator(ctx context.Context, q querier, tenantID uuid.UUID, p LevelPolicy, loc *time.Location,
	types pointTypeSet,
) (*evaluator, error) {
	ev := &evaluator{tenantID: tenantID, policy: p, loc: loc, types: types, levels: map[uuid.UUID]*ladderLevel{}}
	rows, err := q.Query(ctx, `
		SELECT id, slug, rank, is_active, min_spend, min_orders FROM levels
		WHERE tenant_id = $1 ORDER BY rank DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	levels, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*ladderLevel, error) {
		l := &ladderLevel{minPoints: map[uuid.UUID]int64{}}
		err := row.Scan(&l.id, &l.slug, &l.rank, &l.active, &l.minSpend, &l.minOrders)
		return l, err
	})
	if err != nil {
		return nil, err
	}
	for _, l := range levels {
		ev.levels[l.id] = l
		if l.active {
			ev.active = append(ev.active, l)
		}
	}
	rows, err = q.Query(ctx,
		`SELECT level_id, point_type_id, min_points FROM level_requirements WHERE tenant_id = $1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			levelID, typeID uuid.UUID
			min             int64
		)
		if err := rows.Scan(&levelID, &typeID, &min); err != nil {
			return nil, err
		}
		if l, ok := ev.levels[levelID]; ok {
			l.minPoints[typeID] = min
		}
	}
	return ev, rows.Err()
}

// best returns the highest active level the activity meets, or nil.
func (ev *evaluator) best(a activity) *ladderLevel {
	for _, l := range ev.active {
		if l.metBy(a) {
			return l
		}
	}
	return nil
}

// windowActivity sums an entity's spend, orders and points earned with created_at in [from, to].
func windowActivity(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, entityID string, from, to time.Time) (activity, error) {
	a := activity{points: map[uuid.UUID]int64{}}
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(spend), 0)::bigint, COALESCE(SUM(orders), 0)::bigint FROM ledger_entries
		WHERE tenant_id = $1 AND entity_id = $2 AND (spend <> 0 OR orders <> 0)
			AND created_at >= $3 AND created_at <= $4`,
		tenantID, entityID, from, to,
	).Scan(&a.spend, &a.orders); err != nil {
		return a, err
	}
	rows, err := tx.Query(ctx, `
		SELECT point_type_id, COALESCE(SUM(delta), 0)::bigint FROM ledger_lines
		WHERE tenant_id = $1 AND entity_id = $2 AND created_at >= $3 AND created_at <= $4
			AND (entry_type = 'adjustment' OR (entry_type = 'rule' AND delta > 0))
		GROUP BY point_type_id`,
		tenantID, entityID, from, to)
	if err != nil {
		return a, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id uuid.UUID
			n  int64
		)
		if err := rows.Scan(&id, &n); err != nil {
			return a, err
		}
		a.points[id] = n
	}
	return a, rows.Err()
}

// clamp floors every figure at zero; refunds and corrections can push a window negative.
func (a activity) clamp() activity {
	out := activity{spend: max(a.spend, 0), orders: max(a.orders, 0), points: make(map[uuid.UUID]int64, len(a.points))}
	for id, n := range a.points {
		out.points[id] = max(n, 0)
	}
	return out
}

// evaluate recomputes an entity's qualifying activity and moves it to the level that activity
// earns, inside tx. asOf defaults to the database clock.
//
// Under the calendar basis the target is the better of the levels met by the current and the
// previous period, so a level reached in one period is kept through the next. Upgrades apply
// immediately. When the entity no longer meets its current level it keeps the level and the
// grace clock (below_level_since) starts; a downgrade applies only if the policy allows
// downgrades, grace_days have passed and the level has been held for min_tenure_days.
// Qualifying again stops the clock. An entity whose level was deactivated or deleted moves
// straight to the level it qualifies for.
func (ev *evaluator) evaluate(ctx context.Context, tx pgx.Tx, entityID string, asOf *time.Time) (*LevelChange, error) {
	var (
		lifetimeSpend, lifetimeOrders, prevSpend, prevOrders int64
		curID                                                *uuid.UUID
		levelSince, belowSince                               *time.Time
		now                                                  time.Time
	)
	if err := tx.QueryRow(ctx, `
		SELECT lifetime_spend, lifetime_orders, qualifying_spend, qualifying_orders, current_level_id,
			level_since, below_level_since, COALESCE($3::timestamptz, now())
		FROM entity_profiles WHERE tenant_id = $1 AND entity_id = $2
		FOR UPDATE`,
		ev.tenantID, entityID, asOf,
	).Scan(&lifetimeSpend, &lifetimeOrders, &prevSpend, &prevOrders, &curID, &levelSince, &belowSince, &now); err != nil {
		return nil, err
	}
	type earned struct{ lifetime, qualifying int64 }
	rows, err := tx.Query(ctx, `
		SELECT point_type_id, lifetime_earned, qualifying_earned FROM entity_balances
		WHERE tenant_id = $1 AND entity_id = $2`, ev.tenantID, entityID)
	if err != nil {
		return nil, err
	}
	balances := map[uuid.UUID]earned{}
	for rows.Next() {
		var (
			id uuid.UUID
			e  earned
		)
		if err := rows.Scan(&id, &e.lifetime, &e.qualifying); err != nil {
			rows.Close()
			return nil, err
		}
		balances[id] = e
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var (
		cur  activity
		prev *activity
	)
	switch p := ev.policy; {
	case p.Basis == LevelBasisRolling && p.WindowDays != nil:
		if cur, err = windowActivity(ctx, tx, ev.tenantID, entityID, now.AddDate(0, 0, -*p.WindowDays), now); err != nil {
			return nil, err
		}
	case p.Basis == LevelBasisCalendar && p.PeriodMonths != nil:
		start, _ := calendarPeriod(now, ev.loc, *p.PeriodMonths)
		if cur, err = windowActivity(ctx, tx, ev.tenantID, entityID, start, now); err != nil {
			return nil, err
		}
		prevStart, _ := calendarPeriod(start.Add(-time.Nanosecond), ev.loc, *p.PeriodMonths)
		last, err := windowActivity(ctx, tx, ev.tenantID, entityID, prevStart, start.Add(-time.Microsecond))
		if err != nil {
			return nil, err
		}
		last = last.clamp()
		prev = &last
	default:
		cur = activity{spend: lifetimeSpend, orders: lifetimeOrders, points: map[uuid.UUID]int64{}}
		for id, e := range balances {
			cur.points[id] = e.lifetime
		}
	}
	cur = cur.clamp()

	target := ev.best(cur)
	if prev != nil {
		if t := ev.best(*prev); t != nil && (target == nil || t.rank > target.rank) {
			target = t
		}
	}
	var current *ladderLevel
	if curID != nil {
		current = ev.levels[*curID]
	}

	next, newBelow := current, belowSince
	switch {
	case current == nil || !current.active:
		next, newBelow = target, nil
	case target != nil && target.rank >= current.rank:
		next, newBelow = target, nil
	default:
		if newBelow == nil {
			newBelow = &now
		}
		grace := time.Duration(ev.policy.GraceDays) * 24 * time.Hour
		tenured := levelSince == nil || !now.Before(levelSince.AddDate(0, 0, ev.policy.MinTenureDays))
		if ev.policy.Downgrade && !now.Before(newBelow.Add(grace)) && tenured {
			next, newBelow = target, nil
		}
	}

	for id, e := range balances {
		if q := cur.points[id]; q != e.qualifying {
			if _, err := tx.Exec(ctx, `
				UPDATE entity_balances SET qualifying_earned = $4
				WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3`,
				ev.tenantID, entityID, id, q); err != nil {
				return nil, err
			}
		}
	}

	// Both point into ev.levels, so pointer equality is level identity.
	changed := next != current
	if !changed && cur.spend == prevSpend && cur.orders == prevOrders && sameTime(newBelow, belowSince) {
		return nil, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE entity_profiles
		SET qualifying_spend = $3,
			qualifying_orders = $4,
			current_level_id = $5,
			level_since = CASE WHEN $6 THEN $7 ELSE level_since END,
			below_level_since = $8,
			updated_at = now()
		WHERE tenant_id = $1 AND entity_id = $2`,
		ev.tenantID, entityID, cur.spend, cur.orders, levelID(next), changed, nullIfNil(levelID(next), now), newBelow,
	); err != nil {
		return nil, err
	}
	if !changed {
		return nil, nil
	}

	direction := LevelUp
	if next == nil || (current != nil && next.rank < current.rank) {
		direction = LevelDown
	}
	c := LevelChange{
		EntityID: entityID, Direction: direction,
		FromLevelID: levelID(current), FromLevelSlug: levelSlug(current),
		ToLevelID: levelID(next), ToLevelSlug: levelSlug(next),
		QualifyingSpend: cur.spend, QualifyingOrders: cur.orders, QualifyingPoints: map[string]int64{},
	}
	for id := range balances {
		if pt, ok := ev.types.byID[id]; ok {
			c.QualifyingPoints[pt.Slug] = cur.points[id]
		}
	}
	points, err := json.Marshal(c.QualifyingPoints)
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO level_changes (tenant_id, entity_id, direction, from_level_id, from_level_slug,
			to_level_id, to_level_slug, qualifying_spend, qualifying_orders, qualifying_points, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, created_at`,
		ev.tenantID, entityID, c.Direction, c.FromLevelID, c.FromLevelSlug, c.ToLevelID, c.ToLevelSlug,
		c.QualifyingSpend, c.QualifyingOrders, points, now,
	).Scan(&c.Sequence, &c.CreatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func levelID(l *ladderLevel) *uuid.UUID {
	if l == nil {
		return nil
	}
	return &l.id
}

func levelSlug(l *ladderLevel) *string {
	if l == nil {
		return nil
	}
	return &l.slug
}

// ReevaluateLevels evaluates the levels of a tenant's entities, each in its own transaction.
// With timeSensitiveOnly it skips entities whose level can't change with the passage of time
// (no level and no qualifying activity). It returns how many entities changed level.
func (s *Store) ReevaluateLevels(ctx context.Context, tenant Tenant, asOf *time.Time, timeSensitiveOnly bool,
) (evaluated, changed int, err error) {
	const batchSize = 500
	types, err := loadPointTypeSet(ctx, s.pool, tenant.ID)
	if err != nil {
		return 0, 0, err
	}
	ev, err := newEvaluator(ctx, s.pool, tenant.ID, tenant.LevelPolicy, location(tenant.Timezone), types)
	if err != nil {
		return 0, 0, err
	}
	filter := ""
	if timeSensitiveOnly {
		filter = `AND (current_level_id IS NOT NULL OR qualifying_spend <> 0 OR qualifying_orders <> 0
			OR EXISTS (SELECT 1 FROM entity_balances b WHERE b.tenant_id = entity_profiles.tenant_id
				AND b.entity_id = entity_profiles.entity_id AND b.qualifying_earned <> 0))`
	}
	after := ""
	for {
		rows, err := s.pool.Query(ctx, fmt.Sprintf(`
			SELECT entity_id FROM entity_profiles
			WHERE tenant_id = $1 AND entity_id > $2 %s
			ORDER BY entity_id LIMIT $3`, filter),
			tenant.ID, after, batchSize)
		if err != nil {
			return evaluated, changed, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return evaluated, changed, err
		}
		for _, id := range ids {
			var c *LevelChange
			if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
				var err error
				c, err = ev.evaluate(ctx, tx, id, asOf)
				return err
			}); err != nil {
				return evaluated, changed, err
			}
			evaluated++
			if c != nil {
				changed++
			}
		}
		if len(ids) < batchSize {
			return evaluated, changed, nil
		}
		after = ids[len(ids)-1]
	}
}

// levelSweepLockID is a Postgres advisory lock key so only one instance sweeps at a time.
const levelSweepLockID = 7_316_904_413

// SweepLevels re-evaluates time-sensitive levels for every tenant whose policy lets levels
// change as time passes (rolling or calendar basis, or downgrades enabled). If another
// instance holds the sweep lock it returns immediately with ran false.
func (s *Store) SweepLevels(ctx context.Context) (ran bool, changed int, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, 0, err
	}
	defer conn.Release()
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", levelSweepLockID).Scan(&ran); err != nil || !ran {
		return false, 0, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", levelSweepLockID) //nolint:errcheck

	rows, err := s.pool.Query(ctx,
		`SELECT `+tenantColumns+` FROM tenants WHERE level_basis IN ($1, $2) OR level_downgrade`,
		LevelBasisRolling, LevelBasisCalendar)
	if err != nil {
		return true, 0, err
	}
	tenants, err := collect(rows, scanTenant)
	if err != nil {
		return true, 0, err
	}
	for _, t := range tenants {
		_, n, err := s.ReevaluateLevels(ctx, t, nil, true)
		changed += n
		if err != nil {
			return true, changed, fmt.Errorf("tenant %s: %w", t.ID, err)
		}
	}
	return true, changed, nil
}

type LevelChangeFilter struct {
	EntityID string
	// Cursor is a keyset cursor on the change sequence number.
	Cursor    *int64
	Ascending bool
	Limit     int
}

func (s *Store) ListLevelChanges(ctx context.Context, tenantID uuid.UUID, f LevelChangeFilter) ([]LevelChange, error) {
	order, cmp := "DESC", "<"
	if f.Ascending {
		order, cmp = "ASC", ">"
	}
	args := []any{tenantID, f.EntityID, f.Limit}
	cursor := ""
	if f.Cursor != nil {
		args = append(args, *f.Cursor)
		cursor = fmt.Sprintf("AND id %s $4", cmp)
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT id, entity_id, direction, from_level_id, from_level_slug, to_level_id, to_level_slug,
			qualifying_spend, qualifying_orders, qualifying_points, created_at
		FROM level_changes
		WHERE tenant_id = $1 AND entity_id = $2 %s
		ORDER BY id %s LIMIT $3`, cursor, order), args...)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (LevelChange, error) {
		var (
			c      LevelChange
			points []byte
		)
		if err := row.Scan(&c.Sequence, &c.EntityID, &c.Direction, &c.FromLevelID, &c.FromLevelSlug,
			&c.ToLevelID, &c.ToLevelSlug, &c.QualifyingSpend, &c.QualifyingOrders, &points, &c.CreatedAt); err != nil {
			return c, err
		}
		c.QualifyingPoints = map[string]int64{}
		return c, json.Unmarshal(points, &c.QualifyingPoints)
	})
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

// nullIfNil returns nil when id is nil and t otherwise, so a profile without a level has no level_since.
func nullIfNil(id *uuid.UUID, t time.Time) *time.Time {
	if id == nil {
		return nil
	}
	return &t
}
