package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const tierColumns = "id, tenant_id, slug, name, description, is_active, created_at, updated_at"

func scanTier(row pgx.Row) (Tier, error) {
	var t Tier
	err := row.Scan(&t.ID, &t.TenantID, &t.Slug, &t.Name, &t.Description, &t.IsActive, &t.CreatedAt, &t.UpdatedAt)
	return t, translate(err)
}

func (s *Store) CreateTier(ctx context.Context, t Tier) (Tier, error) {
	return scanTier(s.pool.QueryRow(ctx, `
		INSERT INTO tiers (tenant_id, slug, name, description, is_active)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+tierColumns,
		t.TenantID, t.Slug, t.Name, t.Description, t.IsActive))
}

func (s *Store) ListTiers(ctx context.Context, tenantID uuid.UUID) ([]Tier, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+tierColumns+` FROM tiers WHERE tenant_id = $1 ORDER BY created_at, id`, tenantID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanTier)
}

func (s *Store) GetTier(ctx context.Context, tenantID, tierID uuid.UUID) (Tier, error) {
	return scanTier(s.pool.QueryRow(ctx,
		`SELECT `+tierColumns+` FROM tiers WHERE tenant_id = $1 AND id = $2`, tenantID, tierID))
}

// UpdateTier loads the tier under a row lock, lets mutate change it, and saves the result.
func (s *Store) UpdateTier(ctx context.Context, tenantID, tierID uuid.UUID, mutate func(*Tier) error) (Tier, error) {
	var t Tier
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		t, err = scanTier(tx.QueryRow(ctx,
			`SELECT `+tierColumns+` FROM tiers WHERE tenant_id = $1 AND id = $2 FOR UPDATE`, tenantID, tierID))
		if err != nil {
			return err
		}
		if err := mutate(&t); err != nil {
			return err
		}
		t, err = scanTier(tx.QueryRow(ctx, `
			UPDATE tiers SET slug = $3, name = $4, description = $5, is_active = $6, updated_at = now()
			WHERE tenant_id = $1 AND id = $2
			RETURNING `+tierColumns,
			tenantID, tierID, t.Slug, t.Name, t.Description, t.IsActive))
		return err
	})
	return t, err
}

// DeleteTier removes a tier and (via cascade) its rules. Ledger history is kept.
func (s *Store) DeleteTier(ctx context.Context, tenantID, tierID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM tiers WHERE tenant_id = $1 AND id = $2`, tenantID, tierID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
