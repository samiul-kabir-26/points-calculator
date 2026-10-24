-- Membership levels (e.g. Silver, Gold) are computed from an entity's qualifying spend.
-- They are separate from tiers, which stay rule packages; a level may point at one.

-- Per-tenant qualification policy.
ALTER TABLE tenants
    -- lifetime: all spend counts. rolling: only spend in the last level_window_days.
    ADD COLUMN level_basis       text    NOT NULL DEFAULT 'lifetime' CHECK (level_basis IN ('lifetime', 'rolling')),
    ADD COLUMN level_window_days integer CHECK (level_window_days BETWEEN 1 AND 3650),
    -- Whether an entity can drop to a lower level once its qualifying spend falls below the threshold.
    ADD COLUMN level_downgrade   boolean NOT NULL DEFAULT false,
    -- How long an entity keeps its level after it stops qualifying, before a downgrade applies.
    ADD COLUMN level_grace_days  integer NOT NULL DEFAULT 0 CHECK (level_grace_days BETWEEN 0 AND 3650),
    ADD CONSTRAINT tenants_level_window CHECK ((level_basis = 'rolling') = (level_window_days IS NOT NULL));

CREATE TABLE levels (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    slug        text NOT NULL,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    -- Qualifying spend, in the tenant's smallest currency unit, needed to reach this level.
    min_spend   bigint NOT NULL CHECK (min_spend >= 0),
    -- Optional rule package used when an execution doesn't name a tier.
    tier_id     uuid,
    is_active   boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT levels_tenant_slug_key UNIQUE (tenant_id, slug),
    -- Thresholds are unique so levels have an unambiguous order.
    CONSTRAINT levels_tenant_min_spend_key UNIQUE (tenant_id, min_spend),
    CONSTRAINT levels_tenant_id_key UNIQUE (tenant_id, id),
    CONSTRAINT levels_tier_fkey FOREIGN KEY (tenant_id, tier_id)
        REFERENCES tiers (tenant_id, id) ON DELETE SET NULL (tier_id)
);

ALTER TABLE entity_profiles
    ADD COLUMN lifetime_spend    bigint NOT NULL DEFAULT 0,
    -- Spend counted toward the level under the tenant's basis, as of the last evaluation.
    ADD COLUMN qualifying_spend  bigint NOT NULL DEFAULT 0,
    ADD COLUMN current_level_id  uuid,
    ADD COLUMN level_since       timestamptz,
    -- Set while the entity no longer meets its current level's threshold (the grace clock).
    ADD COLUMN below_level_since timestamptz,
    ADD CONSTRAINT entity_profiles_lifetime_spend_non_negative CHECK (lifetime_spend >= 0),
    ADD CONSTRAINT entity_profiles_level_fkey FOREIGN KEY (tenant_id, current_level_id)
        REFERENCES levels (tenant_id, id) ON DELETE SET NULL (current_level_id);

CREATE INDEX entity_profiles_level_idx ON entity_profiles (tenant_id, current_level_id);

-- Spend attached to a transaction; negative for refunds. Independent of point_delta.
ALTER TABLE ledger_entries ADD COLUMN spend bigint NOT NULL DEFAULT 0;

CREATE INDEX ledger_entries_spend_idx ON ledger_entries (tenant_id, entity_id, created_at) WHERE spend <> 0;

CREATE TABLE level_changes (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id        uuid NOT NULL REFERENCES tenants (id),
    entity_id        text NOT NULL,
    direction        text NOT NULL CHECK (direction IN ('up', 'down')),
    -- Denormalized (no FK) so history survives level deletion.
    from_level_id    uuid,
    from_level_slug  text,
    to_level_id      uuid,
    to_level_slug    text,
    qualifying_spend bigint NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX level_changes_tenant_entity_idx ON level_changes (tenant_id, entity_id, id);
