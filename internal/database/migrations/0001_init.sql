CREATE TABLE tenants (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name             text NOT NULL,
    -- The identifier the tenant attaches points to (e.g. user_id, wallet_address).
    architecture_key text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id  uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    name       text NOT NULL DEFAULT '',
    prefix     text NOT NULL,
    key_hash   bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);

CREATE INDEX api_keys_tenant_idx ON api_keys (tenant_id);

CREATE TABLE tiers (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    slug        text NOT NULL,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    is_active   boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tiers_tenant_slug_key UNIQUE (tenant_id, slug),
    -- Target for composite foreign keys so children can't point at another tenant's tier.
    CONSTRAINT tiers_tenant_id_key UNIQUE (tenant_id, id)
);

CREATE TABLE rules (
    id                        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 uuid NOT NULL,
    tier_id                   uuid NOT NULL,
    slug                      text NOT NULL,
    name                      text NOT NULL,
    description               text NOT NULL DEFAULT '',
    point_delta               bigint NOT NULL CHECK (point_delta <> 0),
    cooldown_seconds          integer NOT NULL DEFAULT 0 CHECK (cooldown_seconds >= 0),
    rate_limit_max            integer NOT NULL DEFAULT 0 CHECK (rate_limit_max >= 0),
    rate_limit_window_seconds integer NOT NULL DEFAULT 0 CHECK (rate_limit_window_seconds >= 0),
    is_active                 boolean NOT NULL DEFAULT true,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT rules_tier_fkey FOREIGN KEY (tenant_id, tier_id)
        REFERENCES tiers (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT rules_tier_slug_key UNIQUE (tier_id, slug),
    CONSTRAINT rules_rate_limit_pair CHECK ((rate_limit_max = 0) = (rate_limit_window_seconds = 0))
);

CREATE INDEX rules_tenant_tier_idx ON rules (tenant_id, tier_id);

CREATE TABLE entity_profiles (
    tenant_id       uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    entity_id       text NOT NULL,
    balance         bigint NOT NULL DEFAULT 0,
    current_tier_id uuid REFERENCES tiers (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, entity_id),
    CONSTRAINT entity_profiles_balance_non_negative CHECK (balance >= 0)
);

CREATE INDEX entity_profiles_tier_idx ON entity_profiles (tenant_id, current_tier_id);

CREATE TABLE ledger_entries (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id  uuid NOT NULL DEFAULT gen_random_uuid() UNIQUE,
    -- No cascade: the ledger is the audit trail and must outlive configuration changes.
    tenant_id       uuid NOT NULL REFERENCES tenants (id),
    entity_id       text NOT NULL,
    entry_type      text NOT NULL CHECK (entry_type IN ('rule', 'adjustment')),
    -- Tier/rule are denormalized (no FK) so history survives tier and rule deletion.
    tier_id         uuid,
    tier_slug       text,
    rule_id         uuid,
    rule_slug       text,
    point_delta     bigint NOT NULL,
    balance_before  bigint NOT NULL,
    balance_after   bigint NOT NULL,
    idempotency_key text,
    reason          text,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX ledger_entries_idempotency_key
    ON ledger_entries (tenant_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX ledger_entries_tenant_entity_idx ON ledger_entries (tenant_id, entity_id, id);
CREATE INDEX ledger_entries_tenant_idx ON ledger_entries (tenant_id, id);
CREATE INDEX ledger_entries_tenant_created_idx ON ledger_entries (tenant_id, created_at);

CREATE FUNCTION ledger_entries_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only; record a compensating entry instead'
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER ledger_entries_no_update_delete
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();

CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_append_only();
