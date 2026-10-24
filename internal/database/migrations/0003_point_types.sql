-- Multiple point types per tenant, rule effects (fixed, per-spend, caller-supplied and streak
-- amounts), expiring lots, per-period caps, check-in streaks, and levels that combine spend,
-- order count and points earned. See docs/MULTI_CURRENCY_DESIGN.md.

-- Tenant calendar and the extended level policy.
ALTER TABLE tenants
    -- IANA time zone used for day/week/month boundaries, month-end expiry and streak days.
    ADD COLUMN timezone              text    NOT NULL DEFAULT 'UTC',
    -- calendar basis: qualifying activity is counted per fixed period of this many months,
    -- aligned to 1 January.
    ADD COLUMN level_period_months   integer CHECK (level_period_months IN (1, 2, 3, 4, 6, 12)),
    -- A level can't be lost through a downgrade until it has been held this long.
    ADD COLUMN level_min_tenure_days integer NOT NULL DEFAULT 0 CHECK (level_min_tenure_days BETWEEN 0 AND 3650),
    DROP CONSTRAINT tenants_level_basis_check,
    ADD CONSTRAINT tenants_level_basis_check CHECK (level_basis IN ('lifetime', 'rolling', 'calendar')),
    ADD CONSTRAINT tenants_level_period CHECK ((level_basis = 'calendar') = (level_period_months IS NOT NULL));

-- A currency the tenant defines (coins, gems, xp, ...). Exactly one is primary: the legacy
-- single-balance fields (rule point_delta, profile balance, entry point_delta) refer to it.
CREATE TABLE point_types (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    uuid NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    slug         text NOT NULL,
    name         text NOT NULL,
    description  text NOT NULL DEFAULT '',
    is_primary   boolean NOT NULL DEFAULT false,
    -- Non-spendable types (e.g. XP) can only be awarded by rules; adjustments may still correct them.
    spendable    boolean NOT NULL DEFAULT true,
    -- Default expiry of earned points: never, N days after earning, or the end of the Nth
    -- calendar month counting the earning month as the first.
    expiry_mode  text NOT NULL DEFAULT 'never' CHECK (expiry_mode IN ('never', 'days', 'end_of_month')),
    expiry_value integer CHECK (expiry_value BETWEEN 1 AND 3650),
    -- Per-entity caps on rule credits/debits per day, week or month: [{direction, period, max}].
    limits       jsonb NOT NULL DEFAULT '[]',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT point_types_tenant_slug_key UNIQUE (tenant_id, slug),
    CONSTRAINT point_types_tenant_id_key UNIQUE (tenant_id, id),
    CONSTRAINT point_types_expiry CHECK ((expiry_mode = 'never') = (expiry_value IS NULL))
);

CREATE UNIQUE INDEX point_types_one_primary ON point_types (tenant_id) WHERE is_primary;

INSERT INTO point_types (tenant_id, slug, name, is_primary)
SELECT id, 'points', 'Points', true FROM tenants;

-- What a rule does to each point type. Amounts are magnitudes; direction gives the sign.
ALTER TABLE rules
    ADD CONSTRAINT rules_tenant_id_key UNIQUE (tenant_id, id),
    -- Added to the entity's order count: 1 for a completed order, -1 for a cancellation.
    ADD COLUMN order_delta integer NOT NULL DEFAULT 0 CHECK (order_delta BETWEEN -1000 AND 1000);

CREATE TABLE rule_effects (
    rule_id       uuid NOT NULL,
    tenant_id     uuid NOT NULL,
    point_type_id uuid NOT NULL,
    position      integer NOT NULL,
    -- fixed: amount. per_spend: floor(spend * rate_points / rate_per). input: the request's
    -- amount. streak: steps[day in streak], cycling.
    kind          text NOT NULL CHECK (kind IN ('fixed', 'per_spend', 'input', 'streak')),
    direction     text NOT NULL CHECK (direction IN ('credit', 'debit')),
    amount        bigint NOT NULL DEFAULT 0 CHECK (amount >= 0),
    rate_points   bigint NOT NULL DEFAULT 0 CHECK (rate_points >= 0),
    rate_per      bigint NOT NULL DEFAULT 0 CHECK (rate_per >= 0),
    -- Per-transaction bounds; 0 means unbounded.
    min_amount    bigint NOT NULL DEFAULT 0 CHECK (min_amount >= 0),
    max_amount    bigint NOT NULL DEFAULT 0 CHECK (max_amount >= 0),
    steps         bigint[] NOT NULL DEFAULT '{}',
    -- Overrides the point type's default expiry for credits from this rule.
    expiry_mode   text CHECK (expiry_mode IN ('never', 'days', 'end_of_month')),
    expiry_value  integer CHECK (expiry_value BETWEEN 1 AND 3650),
    PRIMARY KEY (rule_id, point_type_id),
    CONSTRAINT rule_effects_rule_fkey FOREIGN KEY (tenant_id, rule_id)
        REFERENCES rules (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT rule_effects_point_type_fkey FOREIGN KEY (tenant_id, point_type_id)
        REFERENCES point_types (tenant_id, id)
);

CREATE INDEX rule_effects_point_type_idx ON rule_effects (tenant_id, point_type_id);

INSERT INTO rule_effects (rule_id, tenant_id, point_type_id, position, kind, direction, amount)
SELECT r.id, r.tenant_id, pt.id, 0, 'fixed',
    CASE WHEN r.point_delta < 0 THEN 'debit' ELSE 'credit' END, abs(r.point_delta)
FROM rules r JOIN point_types pt ON pt.tenant_id = r.tenant_id AND pt.is_primary;

ALTER TABLE rules DROP COLUMN point_delta;

-- One balance per entity and point type.
ALTER TABLE entity_profiles
    ADD COLUMN lifetime_orders   bigint NOT NULL DEFAULT 0,
    -- Orders counted toward the level under the tenant's basis, as of the last evaluation.
    ADD COLUMN qualifying_orders bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT entity_profiles_lifetime_orders_non_negative CHECK (lifetime_orders >= 0);

CREATE TABLE entity_balances (
    tenant_id         uuid NOT NULL,
    entity_id         text NOT NULL,
    point_type_id     uuid NOT NULL,
    balance           bigint NOT NULL DEFAULT 0,
    -- Rule credits plus adjustments; redemptions and expiry don't reduce it.
    lifetime_earned   bigint NOT NULL DEFAULT 0,
    -- lifetime_earned restricted to the level basis window, as of the last evaluation.
    qualifying_earned bigint NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, entity_id, point_type_id),
    CONSTRAINT entity_balances_non_negative CHECK (balance >= 0),
    CONSTRAINT entity_balances_profile_fkey FOREIGN KEY (tenant_id, entity_id)
        REFERENCES entity_profiles (tenant_id, entity_id) ON DELETE CASCADE,
    CONSTRAINT entity_balances_point_type_fkey FOREIGN KEY (tenant_id, point_type_id)
        REFERENCES point_types (tenant_id, id)
);

CREATE INDEX entity_balances_point_type_idx ON entity_balances (tenant_id, point_type_id);

INSERT INTO entity_balances (tenant_id, entity_id, point_type_id, balance, lifetime_earned, qualifying_earned)
SELECT p.tenant_id, p.entity_id, pt.id, p.balance, COALESCE(e.earned, 0), COALESCE(e.earned, 0)
FROM entity_profiles p
JOIN point_types pt ON pt.tenant_id = p.tenant_id AND pt.is_primary
LEFT JOIN (
    SELECT tenant_id, entity_id, SUM(point_delta) AS earned
    FROM ledger_entries
    WHERE entry_type = 'adjustment' OR point_delta > 0
    GROUP BY tenant_id, entity_id
) e ON e.tenant_id = p.tenant_id AND e.entity_id = p.entity_id;

ALTER TABLE entity_profiles DROP COLUMN balance;

-- The expiring part of a balance, bucketed by expiry instant. Empty lots are deleted; the
-- ledger is the audit trail. Invariant: 0 <= sum(remaining) <= entity_balances.balance.
CREATE TABLE point_lots (
    tenant_id     uuid NOT NULL,
    entity_id     text NOT NULL,
    point_type_id uuid NOT NULL,
    expires_at    timestamptz NOT NULL,
    remaining     bigint NOT NULL CHECK (remaining > 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, entity_id, point_type_id, expires_at),
    CONSTRAINT point_lots_balance_fkey FOREIGN KEY (tenant_id, entity_id, point_type_id)
        REFERENCES entity_balances (tenant_id, entity_id, point_type_id) ON DELETE CASCADE
);

CREATE INDEX point_lots_expires_idx ON point_lots (expires_at);

-- Daily check-in streaks, keyed by rule slug so a streak survives the entity moving to another
-- tier that has a rule with the same slug (e.g. a level upgrade).
CREATE TABLE entity_streaks (
    tenant_id uuid NOT NULL,
    entity_id text NOT NULL,
    rule_slug text NOT NULL,
    -- Consecutive local days claimed, including last_day.
    length    integer NOT NULL CHECK (length > 0),
    last_day  date NOT NULL,
    PRIMARY KEY (tenant_id, entity_id, rule_slug),
    CONSTRAINT entity_streaks_profile_fkey FOREIGN KEY (tenant_id, entity_id)
        REFERENCES entity_profiles (tenant_id, entity_id) ON DELETE CASCADE
);

-- Ledger: expiry entries, order counts, caller-supplied amounts, and one line per point type.
-- point_delta / balance_before / balance_after on the entry now summarise the primary type.
ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_entry_type_check,
    ADD CONSTRAINT ledger_entries_entry_type_check CHECK (entry_type IN ('rule', 'adjustment', 'expiry')),
    ADD COLUMN orders       bigint NOT NULL DEFAULT 0,
    ADD COLUMN input_amount bigint;

DROP INDEX ledger_entries_spend_idx;
CREATE INDEX ledger_entries_activity_idx ON ledger_entries (tenant_id, entity_id, created_at)
    WHERE spend <> 0 OR orders <> 0;

CREATE TABLE ledger_lines (
    entry_id        bigint NOT NULL REFERENCES ledger_entries (id),
    tenant_id       uuid NOT NULL,
    entity_id       text NOT NULL,
    -- Copied from the entry so caps and level windows can be summed from this table alone.
    entry_type      text NOT NULL,
    point_type_id   uuid NOT NULL,
    -- Denormalized so history keeps the slug the type had at the time.
    point_type_slug text NOT NULL,
    delta           bigint NOT NULL,
    balance_before  bigint NOT NULL,
    balance_after   bigint NOT NULL,
    created_at      timestamptz NOT NULL,
    PRIMARY KEY (entry_id, point_type_id),
    CONSTRAINT ledger_lines_point_type_fkey FOREIGN KEY (tenant_id, point_type_id)
        REFERENCES point_types (tenant_id, id)
);

CREATE INDEX ledger_lines_entity_type_idx ON ledger_lines (tenant_id, entity_id, point_type_id, created_at);
CREATE INDEX ledger_lines_point_type_idx ON ledger_lines (tenant_id, point_type_id, entry_id);

CREATE TRIGGER ledger_lines_no_update_delete
    BEFORE UPDATE OR DELETE ON ledger_lines
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();

CREATE TRIGGER ledger_lines_no_truncate
    BEFORE TRUNCATE ON ledger_lines
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_entries_append_only();

INSERT INTO ledger_lines (entry_id, tenant_id, entity_id, entry_type, point_type_id, point_type_slug,
    delta, balance_before, balance_after, created_at)
SELECT e.id, e.tenant_id, e.entity_id, e.entry_type, pt.id, pt.slug,
    e.point_delta, e.balance_before, e.balance_after, e.created_at
FROM ledger_entries e JOIN point_types pt ON pt.tenant_id = e.tenant_id AND pt.is_primary
WHERE e.point_delta <> 0;

-- Levels: an explicit rank orders the ladder now that thresholds can mix spend, orders and points.
ALTER TABLE levels
    ADD COLUMN rank       integer,
    ADD COLUMN min_orders bigint NOT NULL DEFAULT 0 CHECK (min_orders >= 0),
    DROP CONSTRAINT levels_tenant_min_spend_key;

UPDATE levels l SET rank = r.rn
FROM (SELECT id, row_number() OVER (PARTITION BY tenant_id ORDER BY min_spend) AS rn FROM levels) r
WHERE r.id = l.id;

ALTER TABLE levels
    ALTER COLUMN rank SET NOT NULL,
    ADD CONSTRAINT levels_tenant_rank_key UNIQUE (tenant_id, rank);

-- Minimum points earned (per point type) within the basis window to qualify for a level.
CREATE TABLE level_requirements (
    level_id      uuid NOT NULL,
    tenant_id     uuid NOT NULL,
    point_type_id uuid NOT NULL,
    min_points    bigint NOT NULL CHECK (min_points >= 0),
    PRIMARY KEY (level_id, point_type_id),
    CONSTRAINT level_requirements_level_fkey FOREIGN KEY (tenant_id, level_id)
        REFERENCES levels (tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT level_requirements_point_type_fkey FOREIGN KEY (tenant_id, point_type_id)
        REFERENCES point_types (tenant_id, id)
);

CREATE INDEX level_requirements_point_type_idx ON level_requirements (tenant_id, point_type_id);

ALTER TABLE level_changes
    ADD COLUMN qualifying_orders bigint NOT NULL DEFAULT 0,
    ADD COLUMN qualifying_points jsonb NOT NULL DEFAULT '{}';
