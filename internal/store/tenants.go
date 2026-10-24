package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const tenantColumns = `id, name, architecture_key, timezone, level_basis, level_window_days, level_period_months,
	level_downgrade, level_grace_days, level_min_tenure_days, created_at, updated_at`

func scanTenant(row pgx.Row) (Tenant, error) {
	var t Tenant
	p := &t.LevelPolicy
	err := row.Scan(&t.ID, &t.Name, &t.ArchitectureKey, &t.Timezone, &p.Basis, &p.WindowDays, &p.PeriodMonths,
		&p.Downgrade, &p.GraceDays, &p.MinTenureDays, &t.CreatedAt, &t.UpdatedAt)
	return t, translate(err)
}

const apiKeyColumns = "id, tenant_id, name, prefix, created_at, revoked_at"

func scanAPIKey(row pgx.Row) (APIKey, error) {
	var k APIKey
	err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &k.CreatedAt, &k.RevokedAt)
	return k, translate(err)
}

// CreateTenant creates a tenant together with its first API key and its primary point type,
// "points" (spendable, never expires), which can be renamed and reconfigured later.
func (s *Store) CreateTenant(ctx context.Context, name string, architectureKey *string, timezone string, keyName, keyPrefix string, keyHash []byte) (Tenant, APIKey, error) {
	var (
		t Tenant
		k APIKey
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		t, err = scanTenant(tx.QueryRow(ctx,
			`INSERT INTO tenants (name, architecture_key, timezone) VALUES ($1, $2, $3) RETURNING `+tenantColumns,
			name, architectureKey, timezone))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO point_types (tenant_id, slug, name, is_primary) VALUES ($1, 'points', 'Points', true)`,
			t.ID); err != nil {
			return err
		}
		k, err = scanAPIKey(tx.QueryRow(ctx,
			`INSERT INTO api_keys (tenant_id, name, prefix, key_hash) VALUES ($1, $2, $3, $4) RETURNING `+apiKeyColumns,
			t.ID, keyName, keyPrefix, keyHash))
		return err
	})
	return t, k, err
}

func (s *Store) GetTenantByAPIKeyHash(ctx context.Context, keyHash []byte) (Tenant, error) {
	return scanTenant(s.pool.QueryRow(ctx, `
		SELECT `+tenantColumns+` FROM tenants
		WHERE id = (SELECT tenant_id FROM api_keys WHERE key_hash = $1 AND revoked_at IS NULL)`, keyHash))
}

// SetTimezone changes the tenant's time zone. It affects future day/week/month boundaries,
// expiry dates and streak days; lots keep the expiry they were given.
func (s *Store) SetTimezone(ctx context.Context, tenantID uuid.UUID, timezone string) (Tenant, error) {
	return scanTenant(s.pool.QueryRow(ctx,
		`UPDATE tenants SET timezone = $2, updated_at = now() WHERE id = $1 RETURNING `+tenantColumns,
		tenantID, timezone))
}

// SetArchitectureKey sets the tenant's entity key. Changing an existing key is
// refused once profiles exist, because stored entity IDs would silently change meaning.
func (s *Store) SetArchitectureKey(ctx context.Context, tenantID uuid.UUID, key string) (Tenant, error) {
	var t Tenant
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := scanTenant(tx.QueryRow(ctx,
			`SELECT `+tenantColumns+` FROM tenants WHERE id = $1 FOR UPDATE`, tenantID))
		if err != nil {
			return err
		}
		if current.ArchitectureKey != nil && *current.ArchitectureKey != key {
			var hasProfiles bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM entity_profiles WHERE tenant_id = $1)`, tenantID,
			).Scan(&hasProfiles); err != nil {
				return err
			}
			if hasProfiles {
				return ErrArchitectureLocked
			}
		}
		t, err = scanTenant(tx.QueryRow(ctx,
			`UPDATE tenants SET architecture_key = $2, updated_at = now() WHERE id = $1 RETURNING `+tenantColumns,
			tenantID, key))
		return err
	})
	return t, err
}

func (s *Store) CreateAPIKey(ctx context.Context, tenantID uuid.UUID, name, prefix string, keyHash []byte) (APIKey, error) {
	return scanAPIKey(s.pool.QueryRow(ctx,
		`INSERT INTO api_keys (tenant_id, name, prefix, key_hash) VALUES ($1, $2, $3, $4) RETURNING `+apiKeyColumns,
		tenantID, name, prefix, keyHash))
}

func (s *Store) ListAPIKeys(ctx context.Context, tenantID uuid.UUID) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE tenant_id = $1 ORDER BY created_at`, tenantID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAPIKey)
}

// RevokeAPIKey revokes a key, refusing to revoke the tenant's last active key
// so a tenant can't lock itself out.
func (s *Store) RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) (APIKey, error) {
	var k APIKey
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock all of the tenant's keys so concurrent revocations can't both pass the check.
		rows, err := tx.Query(ctx,
			`SELECT `+apiKeyColumns+` FROM api_keys WHERE tenant_id = $1 FOR UPDATE`, tenantID)
		if err != nil {
			return err
		}
		keys, err := collect(rows, scanAPIKey)
		if err != nil {
			return err
		}
		active, found := 0, false
		for _, key := range keys {
			if key.RevokedAt == nil {
				active++
			}
			if key.ID == keyID {
				found = true
				k = key
			}
		}
		if !found {
			return ErrNotFound
		}
		if k.RevokedAt != nil {
			return nil
		}
		if active <= 1 {
			return ErrLastActiveAPIKey
		}
		k, err = scanAPIKey(tx.QueryRow(ctx,
			`UPDATE api_keys SET revoked_at = now() WHERE tenant_id = $1 AND id = $2 RETURNING `+apiKeyColumns,
			tenantID, keyID))
		return err
	})
	return k, err
}

// collect drains rows through a single-row scan function.
func collect[T any](rows pgx.Rows, scan func(pgx.Row) (T, error)) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
