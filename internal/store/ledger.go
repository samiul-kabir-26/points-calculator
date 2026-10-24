package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const ledgerColumns = `id, transaction_id, entity_id, entry_type, tier_id, tier_slug, rule_id, rule_slug,
	point_delta, spend, orders, input_amount, balance_before, balance_after, idempotency_key, reason, created_at`

func scanLedgerEntry(row pgx.Row) (LedgerEntry, error) {
	e, err := scanLedgerEntryRaw(row)
	return e, translate(err)
}

// scanLedgerEntryRaw returns the driver error untranslated so callers can inspect constraint names.
func scanLedgerEntryRaw(row pgx.Row) (LedgerEntry, error) {
	var e LedgerEntry
	err := row.Scan(&e.Sequence, &e.TransactionID, &e.EntityID, &e.EntryType, &e.TierID, &e.TierSlug,
		&e.RuleID, &e.RuleSlug, &e.PointDelta, &e.Spend, &e.Orders, &e.InputAmount, &e.BalanceBefore,
		&e.BalanceAfter, &e.IdempotencyKey, &e.Reason, &e.CreatedAt)
	e.Lines = []LedgerLine{}
	return e, err
}

// attachLines loads the ledger lines of the given entries.
func attachLines(ctx context.Context, q querier, tenantID uuid.UUID, entries []LedgerEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ids := make([]int64, len(entries))
	idx := make(map[int64]int, len(entries))
	for i, e := range entries {
		ids[i] = e.Sequence
		idx[e.Sequence] = i
	}
	rows, err := q.Query(ctx, `
		SELECT entry_id, point_type_id, point_type_slug, delta, balance_before, balance_after
		FROM ledger_lines WHERE tenant_id = $1 AND entry_id = ANY($2)
		ORDER BY entry_id, point_type_slug COLLATE "C"`, tenantID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id int64
			l  LedgerLine
		)
		if err := rows.Scan(&id, &l.PointTypeID, &l.PointType, &l.Delta, &l.BalanceBefore, &l.BalanceAfter); err != nil {
			return err
		}
		e := &entries[idx[id]]
		e.Lines = append(e.Lines, l)
	}
	return rows.Err()
}

type NewEntry struct {
	TenantID  uuid.UUID
	EntityID  string
	EntryType string
	// TierID, when set, is recorded on the entry and becomes the profile's current tier.
	TierID   *uuid.UUID
	TierSlug *string
	RuleID   *uuid.UUID
	RuleSlug *string
	// Effects are evaluated in order; each one that moves a non-zero amount becomes a ledger line.
	Effects []Effect
	// Spend counts toward the entity's level; negative for refunds.
	Spend int64
	// Orders is added to the entity's order count; negative for cancellations.
	Orders int64
	// InputAmount is the caller-supplied amount used by EffectInput effects.
	InputAmount    *int64
	IdempotencyKey *string
	Reason         *string
	// Timezone and Levels are the tenant's settings, used for caps, expiry, streaks and levels.
	Timezone string
	Levels   LevelPolicy
}

// Applied is the outcome of AppendEntry.
type Applied struct {
	Entry LedgerEntry
	// LevelChange is set when the entry moved the entity to another level.
	LevelChange *LevelChange
	// Streak is the entity's streak length after this claim, for rules with a streak effect.
	Streak *int
}

// move is a pending change to one point type's balance.
type move struct {
	pt    PointType
	delta int64
	// expiresAt is set for credits that expire.
	expiresAt *time.Time
}

// AppendEntry atomically applies a transaction to an entity in a single database transaction:
// it creates the profile if needed, expires any lots that are due, advances the streak,
// computes each effect (applying per-transaction bounds and per-period caps), updates the
// balances and expiry lots, records the ledger entry with one line per point type, and
// re-evaluates the entity's level.
//
// The first UPDATE takes a row-level lock on the profile, which serializes concurrent writes
// to the same entity: each one sees the balances and cap usage committed by the previous.
// entity_balances' CHECK rejects deductions the entity can't afford.
func (s *Store) AppendEntry(ctx context.Context, e NewEntry) (Applied, error) {
	loc := location(e.Timezone)
	var out Applied
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO entity_profiles (tenant_id, entity_id) VALUES ($1, $2)
			ON CONFLICT (tenant_id, entity_id) DO NOTHING`,
			e.TenantID, e.EntityID); err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, `
			UPDATE entity_profiles
			SET current_tier_id = COALESCE($3, current_tier_id),
				lifetime_spend = lifetime_spend + $4,
				lifetime_orders = lifetime_orders + $5,
				updated_at = now()
			WHERE tenant_id = $1 AND entity_id = $2
			RETURNING now()`,
			e.TenantID, e.EntityID, e.TierID, e.Spend, e.Orders,
		).Scan(&now); err != nil {
			return err
		}
		types, err := loadPointTypeSet(ctx, tx, e.TenantID)
		if err != nil {
			return err
		}
		// Points that have already expired must not be spendable, even before the sweep runs.
		if err := expireDue(ctx, tx, e.TenantID, e.EntityID, types, now); err != nil {
			return err
		}

		streak := 0
		if e.RuleSlug != nil && hasStreak(e.Effects) {
			if streak, err = claimStreak(ctx, tx, e.TenantID, e.EntityID, *e.RuleSlug, now, loc); err != nil {
				return err
			}
			out.Streak = &streak
		}

		moves := make([]move, 0, len(e.Effects))
		for _, ef := range e.Effects {
			pt, err := effectPointType(types, ef)
			if err != nil {
				return err
			}
			amount := effectAmount(ef, e.Spend, e.InputAmount, streak)
			// An unaffordable debit is reported as such, ahead of any cap it would also break.
			if ef.Direction == DirectionDebit && amount > 0 {
				var balance int64
				if err := tx.QueryRow(ctx, `
					SELECT COALESCE((SELECT balance FROM entity_balances
						WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3), 0)`,
					e.TenantID, e.EntityID, pt.ID).Scan(&balance); err != nil {
					return err
				}
				if balance < amount {
					return &BalanceError{PointType: pt.Slug}
				}
			}
			if e.EntryType == EntryTypeRule {
				if amount, err = applyLimits(ctx, tx, e.TenantID, e.EntityID, pt, ef.Direction, amount, now, loc); err != nil {
					return err
				}
			}
			if amount == 0 {
				continue
			}
			m := move{pt: pt, delta: amount}
			if ef.Direction == DirectionDebit {
				m.delta = -amount
			} else {
				mode, value := pt.ExpiryMode, pt.ExpiryValue
				if ef.ExpiryMode != nil {
					mode, value = *ef.ExpiryMode, ef.ExpiryValue
				}
				m.expiresAt = expiresAt(now, loc, mode, value)
			}
			moves = append(moves, m)
		}
		lines, err := applyMoves(ctx, tx, e.TenantID, e.EntityID, e.EntryType, moves)
		if err != nil {
			return err
		}
		if out.Entry, err = insertEntry(ctx, tx, e, lines, types.primary); err != nil {
			return err
		}
		ev, err := newEvaluator(ctx, tx, e.TenantID, e.Levels, loc, types)
		if err != nil {
			return err
		}
		out.LevelChange, err = ev.evaluate(ctx, tx, e.EntityID, nil)
		return err
	})
	if pe, ok := pgErr(err); ok {
		switch {
		case pe.Code == pgCheckViolation && pe.ConstraintName == "entity_profiles_lifetime_spend_non_negative":
			return out, ErrSpendBelowZero
		case pe.Code == pgCheckViolation && pe.ConstraintName == "entity_profiles_lifetime_orders_non_negative":
			return out, ErrOrdersBelowZero
		case pe.Code == pgNumericOutOfRange:
			return out, ErrBalanceOverflow
		case pe.Code == pgUniqueViolation && pe.ConstraintName == "ledger_entries_idempotency_key":
			return out, ErrDuplicateIdempotency
		}
	}
	if err != nil {
		return Applied{}, translateApply(err)
	}
	return out, nil
}

// translateApply keeps domain errors and translates driver errors.
func translateApply(err error) error {
	var (
		be *BalanceError
		le *LimitError
		se *StreakError
		pe *PointTypeError
	)
	if errors.As(err, &be) || errors.As(err, &le) || errors.As(err, &se) || errors.As(err, &pe) {
		return err
	}
	return translate(err)
}

func hasStreak(effects []Effect) bool {
	for _, e := range effects {
		if e.Kind == EffectStreak {
			return true
		}
	}
	return false
}

// effectPointType finds an effect's point type: by ID for saved rule effects, by slug
// (empty meaning primary) for adjustments.
func effectPointType(types pointTypeSet, e Effect) (PointType, error) {
	if e.PointTypeID != uuid.Nil {
		if pt, ok := types.byID[e.PointTypeID]; ok {
			return pt, nil
		}
		return PointType{}, &PointTypeError{Slug: e.PointType, Err: ErrPointTypeNotFound}
	}
	return types.resolve(e.PointType)
}

// effectAmount computes the magnitude an effect moves, before per-period caps. Max clamps
// computed amounts; input amounts are bounds-checked by the caller.
func effectAmount(e Effect, spend int64, input *int64, streak int) int64 {
	var n int64
	switch e.Kind {
	case EffectFixed:
		n = e.Amount
	case EffectPerSpend:
		if spend > 0 && e.RatePer > 0 {
			v := new(big.Int).Mul(big.NewInt(spend), big.NewInt(e.RatePoints))
			v.Quo(v, big.NewInt(e.RatePer))
			n = math.MaxInt64
			if v.IsInt64() {
				n = v.Int64()
			}
		}
	case EffectInput:
		if input != nil {
			n = *input
		}
	case EffectStreak:
		if len(e.Steps) > 0 && streak > 0 {
			n = e.Steps[(streak-1)%len(e.Steps)]
		}
	}
	if e.Kind != EffectInput && e.Max > 0 && n > e.Max {
		n = e.Max
	}
	return n
}

// applyLimits enforces the point type's per-period caps on rule activity. A credit over a cap
// is reduced to what is left of the allowance; a debit over a cap is refused, so a redemption
// never takes points without delivering in full.
func applyLimits(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, entityID string, pt PointType,
	direction string, amount int64, now time.Time, loc *time.Location,
) (int64, error) {
	sign := "delta > 0"
	if direction == DirectionDebit {
		sign = "delta < 0"
	}
	for _, lim := range pt.Limits {
		if amount == 0 {
			return 0, nil
		}
		if lim.Direction != direction {
			continue
		}
		var used int64
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(abs(delta)), 0)::bigint FROM ledger_lines
			WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3 AND entry_type = 'rule'
				AND `+sign+` AND created_at >= $4`,
			tenantID, entityID, pt.ID, periodStart(now, loc, lim.Period),
		).Scan(&used); err != nil {
			return 0, err
		}
		remaining := max(lim.Max-used, 0)
		if amount <= remaining {
			continue
		}
		if direction == DirectionDebit {
			return 0, &LimitError{PointType: pt.Slug, Limit: lim, Remaining: remaining}
		}
		amount = remaining
	}
	return amount, nil
}

// claimStreak records today's claim of a streak rule and returns the streak length. Claiming
// the day after the last claim extends the streak; a longer gap restarts it at 1; a second
// claim on the same local day is refused until the next local midnight. Streaks are keyed by
// rule slug, so they carry over between tiers that share the rule.
func claimStreak(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, entityID, ruleSlug string,
	now time.Time, loc *time.Location,
) (int, error) {
	today := civilDate(now, loc)
	var (
		length int
		last   time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT length, last_day FROM entity_streaks
		WHERE tenant_id = $1 AND entity_id = $2 AND rule_slug = $3 FOR UPDATE`,
		tenantID, entityID, ruleSlug).Scan(&length, &last)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		length = 1
	case err != nil:
		return 0, err
	case last.Equal(today):
		return 0, &StreakError{RetryAfter: startOfDay(now, loc).AddDate(0, 0, 1).Sub(now)}
	case last.Equal(today.AddDate(0, 0, -1)):
		length++
	default:
		length = 1
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO entity_streaks (tenant_id, entity_id, rule_slug, length, last_day)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, entity_id, rule_slug) DO UPDATE SET length = EXCLUDED.length, last_day = EXCLUDED.last_day`,
		tenantID, entityID, ruleSlug, length, today)
	return length, err
}

// applyMoves updates balances and lots and returns the resulting ledger lines. Credits that
// expire are added to the lot for their expiry instant; debits consume lots soonest-expiring
// first (expiry entries have already removed their lots).
func applyMoves(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, entityID, entryType string, moves []move) ([]LedgerLine, error) {
	lines := make([]LedgerLine, 0, len(moves))
	for _, m := range moves {
		var earned int64
		switch {
		case entryType == EntryTypeAdjustment:
			earned = m.delta
		case entryType == EntryTypeRule && m.delta > 0:
			earned = m.delta
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO entity_balances (tenant_id, entity_id, point_type_id) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, tenantID, entityID, m.pt.ID); err != nil {
			return nil, err
		}
		var balance int64
		err := tx.QueryRow(ctx, `
			UPDATE entity_balances
			SET balance = balance + $4, lifetime_earned = lifetime_earned + $5, updated_at = now()
			WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3
			RETURNING balance`,
			tenantID, entityID, m.pt.ID, m.delta, earned).Scan(&balance)
		if pe, ok := pgErr(err); ok && pe.Code == pgCheckViolation && pe.ConstraintName == "entity_balances_non_negative" {
			return nil, &BalanceError{PointType: m.pt.Slug}
		}
		if err != nil {
			return nil, err
		}
		switch {
		case m.delta > 0 && m.expiresAt != nil:
			if _, err := tx.Exec(ctx, `
				INSERT INTO point_lots (tenant_id, entity_id, point_type_id, expires_at, remaining)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (tenant_id, entity_id, point_type_id, expires_at)
				DO UPDATE SET remaining = point_lots.remaining + EXCLUDED.remaining, updated_at = now()`,
				tenantID, entityID, m.pt.ID, *m.expiresAt, m.delta); err != nil {
				return nil, err
			}
		case m.delta < 0 && entryType != EntryTypeExpiry:
			if err := consumeLots(ctx, tx, tenantID, entityID, m.pt.ID, -m.delta); err != nil {
				return nil, err
			}
		}
		lines = append(lines, LedgerLine{
			PointType: m.pt.Slug, PointTypeID: m.pt.ID, Delta: m.delta,
			BalanceBefore: balance - m.delta, BalanceAfter: balance,
		})
	}
	return lines, nil
}

// consumeLots takes amount out of the entity's lots, soonest expiry first. Whatever the lots
// don't cover comes out of the non-expiring part of the balance.
func consumeLots(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, entityID string, pointTypeID uuid.UUID, amount int64) error {
	rows, err := tx.Query(ctx, `
		SELECT expires_at, remaining FROM point_lots
		WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3
		ORDER BY expires_at FOR UPDATE`, tenantID, entityID, pointTypeID)
	if err != nil {
		return err
	}
	type lot struct {
		expiresAt time.Time
		remaining int64
	}
	lots, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (lot, error) {
		var l lot
		err := row.Scan(&l.expiresAt, &l.remaining)
		return l, err
	})
	if err != nil {
		return err
	}
	for _, l := range lots {
		if amount == 0 {
			break
		}
		take := min(l.remaining, amount)
		amount -= take
		if take == l.remaining {
			_, err = tx.Exec(ctx, `
				DELETE FROM point_lots
				WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3 AND expires_at = $4`,
				tenantID, entityID, pointTypeID, l.expiresAt)
		} else {
			_, err = tx.Exec(ctx, `
				UPDATE point_lots SET remaining = remaining - $5, updated_at = now()
				WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3 AND expires_at = $4`,
				tenantID, entityID, pointTypeID, l.expiresAt, take)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// insertEntry records the ledger entry and its lines. The entry's point_delta and balances
// summarise the primary point type, which may be untouched by this entry.
func insertEntry(ctx context.Context, tx pgx.Tx, e NewEntry, lines []LedgerLine, primary PointType) (LedgerEntry, error) {
	var (
		delta, before int64
		found         bool
	)
	for _, l := range lines {
		if l.PointTypeID == primary.ID {
			delta, before, found = l.Delta, l.BalanceBefore, true
		}
	}
	if !found {
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE((SELECT balance FROM entity_balances
				WHERE tenant_id = $1 AND entity_id = $2 AND point_type_id = $3), 0)`,
			e.TenantID, e.EntityID, primary.ID).Scan(&before); err != nil {
			return LedgerEntry{}, err
		}
	}
	// Same order as attachLines, so a fresh response and a replay list lines identically.
	slices.SortFunc(lines, func(a, b LedgerLine) int { return strings.Compare(a.PointType, b.PointType) })
	entry, err := scanLedgerEntryRaw(tx.QueryRow(ctx, `
		INSERT INTO ledger_entries (tenant_id, entity_id, entry_type, tier_id, tier_slug, rule_id, rule_slug,
			point_delta, spend, orders, input_amount, balance_before, balance_after, idempotency_key, reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		RETURNING `+ledgerColumns,
		e.TenantID, e.EntityID, e.EntryType, e.TierID, e.TierSlug, e.RuleID, e.RuleSlug,
		delta, e.Spend, e.Orders, e.InputAmount, before, before+delta, e.IdempotencyKey, e.Reason))
	if err != nil {
		return entry, err
	}
	for _, l := range lines {
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_lines (entry_id, tenant_id, entity_id, entry_type, point_type_id, point_type_slug,
				delta, balance_before, balance_after, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			entry.Sequence, e.TenantID, e.EntityID, e.EntryType, l.PointTypeID, l.PointType,
			l.Delta, l.BalanceBefore, l.BalanceAfter, entry.CreatedAt); err != nil {
			return entry, err
		}
	}
	entry.Lines = lines
	return entry, nil
}

// expireDue removes the entity's lots that expired at or before asOf and debits what was left
// in them with one expiry ledger entry. The caller must hold the profile row lock.
func expireDue(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, entityID string, types pointTypeSet, asOf time.Time) error {
	rows, err := tx.Query(ctx, `
		WITH gone AS (
			DELETE FROM point_lots
			WHERE tenant_id = $1 AND entity_id = $2 AND expires_at <= $3
			RETURNING point_type_id, remaining
		)
		SELECT point_type_id, SUM(remaining)::bigint FROM gone GROUP BY point_type_id ORDER BY point_type_id`,
		tenantID, entityID, asOf)
	if err != nil {
		return err
	}
	type expired struct {
		pointTypeID uuid.UUID
		amount      int64
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (expired, error) {
		var x expired
		err := row.Scan(&x.pointTypeID, &x.amount)
		return x, err
	})
	if err != nil || len(due) == 0 {
		return err
	}
	moves := make([]move, 0, len(due))
	var parts []string
	for _, x := range due {
		pt := types.byID[x.pointTypeID]
		moves = append(moves, move{pt: pt, delta: -x.amount})
		parts = append(parts, fmt.Sprintf("%d %s", x.amount, pt.Slug))
	}
	lines, err := applyMoves(ctx, tx, tenantID, entityID, EntryTypeExpiry, moves)
	if err != nil {
		return err
	}
	reason := "expired: " + strings.Join(parts, ", ")
	_, err = insertEntry(ctx, tx, NewEntry{
		TenantID: tenantID, EntityID: entityID, EntryType: EntryTypeExpiry, Reason: &reason,
	}, lines, types.primary)
	return err
}

// expirySweepLockID is a Postgres advisory lock key so only one instance expires lots at a time.
const expirySweepLockID = 7_316_904_414

// ExpirePoints expires every lot due at asOf (default: now) across all tenants, one entity per
// transaction. If another instance holds the sweep lock it returns immediately with ran false.
func (s *Store) ExpirePoints(ctx context.Context, asOf *time.Time) (ran bool, entities int, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, 0, err
	}
	defer conn.Release()
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", expirySweepLockID).Scan(&ran); err != nil || !ran {
		return false, 0, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", expirySweepLockID) //nolint:errcheck

	const batchSize = 500
	types := map[uuid.UUID]pointTypeSet{}
	for {
		rows, err := s.pool.Query(ctx, `
			SELECT DISTINCT tenant_id, entity_id FROM point_lots
			WHERE expires_at <= COALESCE($1::timestamptz, now())
			LIMIT $2`, asOf, batchSize)
		if err != nil {
			return true, entities, err
		}
		type key struct {
			tenantID uuid.UUID
			entityID string
		}
		due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (key, error) {
			var k key
			err := row.Scan(&k.tenantID, &k.entityID)
			return k, err
		})
		if err != nil {
			return true, entities, err
		}
		for _, k := range due {
			if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
				var now time.Time
				if err := tx.QueryRow(ctx, `
					SELECT COALESCE($3::timestamptz, now()) FROM entity_profiles
					WHERE tenant_id = $1 AND entity_id = $2 FOR UPDATE`,
					k.tenantID, k.entityID, asOf).Scan(&now); err != nil {
					return err
				}
				set, ok := types[k.tenantID]
				if !ok {
					var err error
					if set, err = loadPointTypeSet(ctx, tx, k.tenantID); err != nil {
						return err
					}
					types[k.tenantID] = set
				}
				return expireDue(ctx, tx, k.tenantID, k.entityID, set, now)
			}); err != nil {
				return true, entities, fmt.Errorf("tenant %s entity %q: %w", k.tenantID, k.entityID, err)
			}
			entities++
		}
		if len(due) < batchSize {
			return true, entities, nil
		}
	}
}

// ListExpirations returns the entity's lots that haven't expired yet, soonest first.
func (s *Store) ListExpirations(ctx context.Context, tenantID uuid.UUID, entityID string) ([]Expiration, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pt.slug, l.remaining, l.expires_at
		FROM point_lots l JOIN point_types pt ON pt.tenant_id = l.tenant_id AND pt.id = l.point_type_id
		WHERE l.tenant_id = $1 AND l.entity_id = $2 AND l.expires_at > now()
		ORDER BY l.expires_at, pt.slug`, tenantID, entityID)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(row pgx.Row) (Expiration, error) {
		var x Expiration
		err := row.Scan(&x.PointType, &x.Amount, &x.ExpiresAt)
		return x, err
	})
}

func (s *Store) GetEntryByIdempotencyKey(ctx context.Context, tenantID uuid.UUID, key string) (LedgerEntry, error) {
	e, err := scanLedgerEntry(s.pool.QueryRow(ctx,
		`SELECT `+ledgerColumns+` FROM ledger_entries WHERE tenant_id = $1 AND idempotency_key = $2`,
		tenantID, key))
	if err != nil {
		return e, err
	}
	entries := []LedgerEntry{e}
	err = attachLines(ctx, s.pool, tenantID, entries)
	return entries[0], err
}

type LedgerFilter struct {
	EntityID  *string
	TierSlug  *string
	RuleSlug  *string
	EntryType *string
	// PointType limits results to entries that moved this point type (by current slug).
	PointType *string
	From      *time.Time
	To        *time.Time
	// Cursor is a keyset cursor on the entry sequence number.
	Cursor    *int64
	Ascending bool
	Limit     int
}

func (s *Store) ListEntries(ctx context.Context, tenantID uuid.UUID, f LedgerFilter) ([]LedgerEntry, error) {
	where := []string{"tenant_id = $1"}
	args := []any{tenantID}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.EntityID != nil {
		add("entity_id = $%d", *f.EntityID)
	}
	if f.TierSlug != nil {
		add("tier_slug = $%d", *f.TierSlug)
	}
	if f.RuleSlug != nil {
		add("rule_slug = $%d", *f.RuleSlug)
	}
	if f.EntryType != nil {
		add("entry_type = $%d", *f.EntryType)
	}
	if f.PointType != nil {
		add(`EXISTS (SELECT 1 FROM ledger_lines ll JOIN point_types pt ON pt.id = ll.point_type_id
			WHERE ll.entry_id = ledger_entries.id AND pt.tenant_id = $1 AND pt.slug = $%d)`, *f.PointType)
	}
	if f.From != nil {
		add("created_at >= $%d", *f.From)
	}
	if f.To != nil {
		add("created_at < $%d", *f.To)
	}
	order := "DESC"
	if f.Ascending {
		order = "ASC"
	}
	if f.Cursor != nil {
		if f.Ascending {
			add("id > $%d", *f.Cursor)
		} else {
			add("id < $%d", *f.Cursor)
		}
	}
	args = append(args, f.Limit)
	q := fmt.Sprintf("SELECT %s FROM ledger_entries WHERE %s ORDER BY id %s LIMIT $%d",
		ledgerColumns, strings.Join(where, " AND "), order, len(args))
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	entries, err := collect(rows, scanLedgerEntry)
	if err != nil {
		return nil, err
	}
	return entries, attachLines(ctx, s.pool, tenantID, entries)
}
