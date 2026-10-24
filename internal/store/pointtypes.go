package store

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const pointTypeColumns = `id, tenant_id, slug, name, description, is_primary, spendable, expiry_mode,
	expiry_value, limits, created_at, updated_at`

func scanPointType(row pgx.Row) (PointType, error) {
	var (
		pt     PointType
		limits []byte
	)
	err := row.Scan(&pt.ID, &pt.TenantID, &pt.Slug, &pt.Name, &pt.Description, &pt.IsPrimary, &pt.Spendable,
		&pt.ExpiryMode, &pt.ExpiryValue, &limits, &pt.CreatedAt, &pt.UpdatedAt)
	if err != nil {
		return pt, translate(err)
	}
	pt.Limits = []PointLimit{}
	return pt, json.Unmarshal(limits, &pt.Limits)
}

func limitsJSON(l []PointLimit) ([]byte, error) {
	if l == nil {
		l = []PointLimit{}
	}
	return json.Marshal(l)
}

// CreatePointType inserts a point type. Making it primary demotes the current primary.
func (s *Store) CreatePointType(ctx context.Context, pt PointType) (PointType, error) {
	limits, err := limitsJSON(pt.Limits)
	if err != nil {
		return pt, err
	}
	var out PointType
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if pt.IsPrimary {
			if _, err := tx.Exec(ctx,
				`UPDATE point_types SET is_primary = false, updated_at = now() WHERE tenant_id = $1 AND is_primary`,
				pt.TenantID); err != nil {
				return err
			}
		}
		var err error
		out, err = scanPointType(tx.QueryRow(ctx, `
			INSERT INTO point_types (tenant_id, slug, name, description, is_primary, spendable, expiry_mode,
				expiry_value, limits)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			RETURNING `+pointTypeColumns,
			pt.TenantID, pt.Slug, pt.Name, pt.Description, pt.IsPrimary, pt.Spendable, pt.ExpiryMode,
			pt.ExpiryValue, limits))
		return err
	})
	return out, err
}

// ListPointTypes returns the tenant's point types, primary first.
func (s *Store) ListPointTypes(ctx context.Context, tenantID uuid.UUID) ([]PointType, error) {
	return listPointTypes(ctx, s.pool, tenantID)
}

func listPointTypes(ctx context.Context, q querier, tenantID uuid.UUID) ([]PointType, error) {
	rows, err := q.Query(ctx,
		`SELECT `+pointTypeColumns+` FROM point_types WHERE tenant_id = $1 ORDER BY is_primary DESC, created_at, id`,
		tenantID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanPointType)
}

func (s *Store) GetPointType(ctx context.Context, tenantID, id uuid.UUID) (PointType, error) {
	return scanPointType(s.pool.QueryRow(ctx,
		`SELECT `+pointTypeColumns+` FROM point_types WHERE tenant_id = $1 AND id = $2`, tenantID, id))
}

// UpdatePointType loads the point type under a row lock, lets mutate change it, and saves it.
// Promoting it to primary demotes the current primary; the primary itself can't be demoted
// (promote another type instead). It can't become non-spendable while a rule debits it.
func (s *Store) UpdatePointType(ctx context.Context, tenantID, id uuid.UUID, mutate func(*PointType) error) (PointType, error) {
	var pt PointType
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		cur, err := scanPointType(tx.QueryRow(ctx,
			`SELECT `+pointTypeColumns+` FROM point_types WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		pt = cur
		if err := mutate(&pt); err != nil {
			return err
		}
		if cur.IsPrimary && !pt.IsPrimary {
			return ErrPrimaryPointType
		}
		if !cur.IsPrimary && pt.IsPrimary {
			if _, err := tx.Exec(ctx,
				`UPDATE point_types SET is_primary = false, updated_at = now() WHERE tenant_id = $1 AND is_primary`,
				tenantID); err != nil {
				return err
			}
		}
		if cur.Spendable && !pt.Spendable {
			var debited bool
			if err := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM rule_effects
					WHERE tenant_id = $1 AND point_type_id = $2 AND direction = $3)`,
				tenantID, id, DirectionDebit).Scan(&debited); err != nil {
				return err
			}
			if debited {
				return &PointTypeError{Slug: cur.Slug, Err: ErrPointTypeNotSpendable}
			}
		}
		limits, err := limitsJSON(pt.Limits)
		if err != nil {
			return err
		}
		pt, err = scanPointType(tx.QueryRow(ctx, `
			UPDATE point_types SET slug = $3, name = $4, description = $5, is_primary = $6, spendable = $7,
				expiry_mode = $8, expiry_value = $9, limits = $10, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+pointTypeColumns,
			tenantID, id, pt.Slug, pt.Name, pt.Description, pt.IsPrimary, pt.Spendable, pt.ExpiryMode,
			pt.ExpiryValue, limits))
		return err
	})
	return pt, err
}

// DeletePointType removes a point type that nothing refers to yet. The primary type can't be
// deleted, and once balances, rules, levels or ledger lines use a type it is ErrPointTypeInUse.
func (s *Store) DeletePointType(ctx context.Context, tenantID, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		pt, err := scanPointType(tx.QueryRow(ctx,
			`SELECT `+pointTypeColumns+` FROM point_types WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, id))
		if err != nil {
			return err
		}
		if pt.IsPrimary {
			return ErrPrimaryPointType
		}
		_, err = tx.Exec(ctx, `DELETE FROM point_types WHERE tenant_id = $1 AND id = $2`, tenantID, id)
		// Balances, rule effects, level requirements and ledger lines reference it with RESTRICT.
		if pe, ok := pgErr(err); ok && pe.Code == pgForeignKeyViolation {
			return ErrPointTypeInUse
		}
		return err
	})
}

// pointTypeSet indexes a tenant's point types for use inside a transaction.
type pointTypeSet struct {
	byID    map[uuid.UUID]PointType
	bySlug  map[string]PointType
	primary PointType
}

func loadPointTypeSet(ctx context.Context, q querier, tenantID uuid.UUID) (pointTypeSet, error) {
	types, err := listPointTypes(ctx, q, tenantID)
	if err != nil {
		return pointTypeSet{}, err
	}
	set := pointTypeSet{byID: map[uuid.UUID]PointType{}, bySlug: map[string]PointType{}}
	for _, pt := range types {
		set.byID[pt.ID] = pt
		set.bySlug[pt.Slug] = pt
		if pt.IsPrimary {
			set.primary = pt
		}
	}
	return set, nil
}

// resolve finds a point type by slug, with "" meaning the primary type.
func (set pointTypeSet) resolve(slug string) (PointType, error) {
	if slug == "" {
		return set.primary, nil
	}
	pt, ok := set.bySlug[slug]
	if !ok {
		return pt, &PointTypeError{Slug: slug, Err: ErrPointTypeNotFound}
	}
	return pt, nil
}
