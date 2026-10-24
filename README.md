# Points Calculator

A multi-tenant point calculation engine written in Go. Vendors (tenants) configure point types,
tiers, rules and membership levels through an admin API, then trigger point transactions through a
high-throughput execution API that enforces cooldowns, rate limits, per-period caps and
idempotency, and writes every change to an append-only ledger.

A tenant can run several currencies side by side, for example spendable **coins** that expire
alongside non-spendable **XP**. Rules can award fixed amounts, amounts proportional to spend,
caller-supplied amounts (redemptions, game prizes) or escalating daily-streak rewards. Levels can
require spend, order count and points earned, over a lifetime, rolling or calendar-period window.
[`docs/tenants/`](docs/tenants/) shows the engine configured as **Daraz**, **Shopee** and
**Lazada**, and [`docs/MULTI_CURRENCY_DESIGN.md`](docs/MULTI_CURRENCY_DESIGN.md) is the design
behind these features.

See [`docs/point_calculation_engine_requirements.md`](docs/point_calculation_engine_requirements.md)
for the full requirements, and [`docs/PROJECT_OVERVIEW.md`](docs/PROJECT_OVERVIEW.md) for a
conceptual walkthrough (tiers vs. levels, points vs. spend, request lifecycle, configuration recipes).

## Stack

- Go 1.22 (standard library `net/http` routing, `log/slog`)
- PostgreSQL 15+ (balances, configuration, ledger) via `pgx/v5`
- Redis 6+ (cooldowns, sliding-window throttles, idempotency locks) via `go-redis/v9`

## Running

```sh
# Postgres + Redis + API in Docker
PLATFORM_ADMIN_TOKEN=change-me make up

# Or run the API locally against dockerized dependencies
make deps-up
cp .env.example .env   # adjust as needed
make run
```

Then open **http://localhost:8080/ui/** for the admin console. Sign in with a tenant API key, or
onboard a new tenant from the login screen using the platform admin token. The console covers tier
and rule configuration (including on/off toggles), membership levels and the level policy, the
entity key, API key rotation, profiles (with level, qualifying spend and level history), the
filterable ledger, and a form for test executions and manual adjustments.

To load a sample loyalty program (six tiers with rules, four spend-based levels linked to them,
and a rolling 12-month level policy with a 30-day grace period) into a tenant:

```sh
go run ./cmd/seed -key pce_... -file seeds/loyalty_program.json
```

To create three ready-made tenants modelled on Daraz, Shopee and Lazada (point types, rules,
levels, policy and demo customers), with their API keys saved to `seeds/tenant_keys.local.json`:

```sh
make seed-tenants        # uses PLATFORM_ADMIN_TOKEN from .env
```

The seed is idempotent: it matches point types, tiers, rules and levels by slug, re-evaluates
every profile afterwards, and gives demo transactions idempotency keys so re-runs replay them. It's a single embedded
HTML file (`internal/api/ui/index.html`) that calls the same public API, with no build step.

Migrations are embedded in the binary and applied on startup (`AUTO_MIGRATE=true`); an advisory
lock makes concurrent instance startups safe. See [`.env.example`](.env.example) for all settings.

## Testing

```sh
make test              # unit tests only
make test-integration  # starts Postgres/Redis in Docker, runs everything with -race
```

The integration tests drive the full HTTP stack against real Postgres and Redis and cover:
the end-to-end flow, cooldowns, throttling (including under concurrency), rule/tier toggles,
idempotent replays (including 20 concurrent duplicates), 100 concurrent writes to one entity
with ledger-chain verification, insufficient-balance handling, adjustments, ledger immutability,
cross-tenant isolation, the architecture-key lock, admin validation and pagination, and
membership levels (lifetime and rolling bases, grace-period downgrades, level-linked tiers).

## Authentication

| Caller | Credential |
|---|---|
| Platform operator (onboards tenants) | `Authorization: Bearer $PLATFORM_ADMIN_TOKEN` |
| Tenant (all `/admin`, `/engine`, `/data` routes) | `Authorization: Bearer <api key>` or `X-API-Key: <api key>` |

Tenant API keys are shown once at creation and stored as SHA-256 hashes. Onboarding accepts an
optional `timezone` (IANA name, default `UTC`); day/week/month caps, month-end expiry, streak days
and calendar level periods follow it.

## API

All errors have the shape `{"error": {"code": "...", "message": "...", "request_id": "..."}}`.

### Onboarding

```sh
curl -X POST localhost:8080/platform/tenants -H "Authorization: Bearer $PLATFORM_ADMIN_TOKEN" \
  -d '{"name": "Acme", "architecture_key": "user_id", "timezone": "Asia/Dhaka"}'
# → 201 {"tenant": {...}, "api_key": {"id": "...", "prefix": "pce_AbCd1234", "secret": "pce_..."}}
```

### Management API

| Method & path | Purpose |
|---|---|
| `GET /admin/architecture` | Read the architecture (entity) key |
| `POST /admin/architecture` `{"key": "user_id"}` | Set it. Can't be changed once profiles exist (`409 architecture_locked`) |
| `GET/PATCH /admin/settings` `{"timezone": "Asia/Karachi"}` | Tenant name and time zone |
| `GET/POST /admin/api-keys`, `DELETE /admin/api-keys/{key_id}` | Key rotation. The last active key can't be revoked |
| `POST /admin/point-types` | Create a point type (fields below) |
| `GET /admin/point-types`, `GET /admin/point-types/{id}` | List (primary first) / read |
| `PATCH /admin/point-types/{id}` | Partial update. `"is_primary": true` makes it the primary type |
| `DELETE /admin/point-types/{id}` | Only while unused (`409 point_type_in_use`); the primary can't be deleted |
| `POST /admin/tiers` | Create tier: `slug`, `name`, `description?`, `is_active?` (default `true`) |
| `GET /admin/tiers`, `GET /admin/tiers/{tier_id}` | List / read |
| `PATCH /admin/tiers/{tier_id}` | Partial update |
| `PATCH /admin/tiers/{tier_id}/status` `{"is_active": false}` | Toggle |
| `DELETE /admin/tiers/{tier_id}` | Delete tier and its rules (ledger history is kept) |
| `POST /admin/tiers/{tier_id}/rules` | Create rule (fields below) |
| `GET /admin/tiers/{tier_id}/rules[/{rule_id}]` | List / read |
| `PATCH /admin/tiers/{tier_id}/rules/{rule_id}` | Partial update |
| `PATCH /admin/tiers/{tier_id}/rules/{rule_id}/status` `{"is_active": false}` | Toggle ON/OFF |
| `DELETE /admin/tiers/{tier_id}/rules/{rule_id}` | Delete |
| `GET /admin/level-policy`, `PATCH /admin/level-policy` | Read / change how spend becomes a level (fields below) |
| `POST /admin/levels` | Create level: `slug`, `name`, `rank`, `min_spend?`, `min_orders?`, `min_points?`, `description?`, `tier_id?`, `is_active?` |
| `GET /admin/levels`, `GET /admin/levels/{level_id}` | List (lowest rank first) / read |
| `PATCH /admin/levels/{level_id}` | Partial update; `"tier_id": null` unlinks the tier |
| `PATCH /admin/levels/{level_id}/status` `{"is_active": false}` | Toggle |
| `DELETE /admin/levels/{level_id}` | Delete (level history is kept) |
| `POST /admin/levels/reevaluate` | Re-evaluate every entity now, e.g. after changing thresholds or policy |
| `POST /admin/adjustments` | Compensating transaction: `entity_id`, `points` (±), `point_type?` (default primary), `spend?` (±), `orders?` (±), `reason`, `idempotency_key?`. At least one of `points`/`spend`/`orders` must be non-zero |

### Point types

Every tenant starts with one primary type, `points` (spendable, never expires). Add more, rename
it, or make another type primary.

| Field | Meaning |
|---|---|
| `slug`, `name`, `description` | Identity and display |
| `is_primary` | Exactly one per tenant. `point_delta` on rules, `points` on adjustments, `balance` on profiles and `point_delta`/`balance_before`/`balance_after` on ledger entries refer to it |
| `spendable` | `false` (e.g. XP): rules can only award it. Adjustments can still correct it; no balance can go below zero |
| `expiry_mode`, `expiry_value` | `never`; `days` + N (kept through the Nth day after earning); `end_of_month` + N (kept through the last day of the Nth month, counting the earning month as the first). Expiry is at local midnight |
| `limits` | Per-entity caps on rule activity: `[{"direction": "credit"\|"debit", "period": "day"\|"week"\|"month", "max": N}]`. Credits over a cap are reduced to what's left; debits over a cap are refused (`point_limit_exceeded`). Adjustments ignore caps |

Points that can expire are kept in **lots** (one per expiry date). Redemptions use the lots that
expire soonest first, then the non-expiring part of the balance. Expired points are removed by an
`expiry` ledger entry, both inside the entity's next transaction and by a background sweep
(`EXPIRY_SWEEP_INTERVAL`, default 15m).

```sh
curl -X POST localhost:8080/admin/point-types -H "Authorization: Bearer $KEY" -d '{
  "slug": "coins", "name": "Coins", "expiry_mode": "end_of_month", "expiry_value": 3,
  "limits": [{"direction": "debit", "period": "day", "max": 30000}]
}'
curl -X POST localhost:8080/admin/point-types -H "Authorization: Bearer $KEY" \
  -d '{"slug": "xp", "name": "Experience", "spendable": false}'
```

### Rules

| Field | Meaning |
|---|---|
| `slug` | Identifier used in execution calls, unique within the tier (`^[a-z0-9][a-z0-9_-]{0,63}$`) |
| `name`, `description` | Display text |
| `effects` | What the rule does to each point type (below); at most one effect per type |
| `point_delta` | Shorthand for a single fixed effect on the primary type (negative to debit). Use instead of `effects` |
| `order_delta` | Added to the entity's order count: `1` for a completed order, `-1` for a cancellation |
| `cooldown_seconds` | Minimum gap between triggers by the same entity; `0` disables |
| `rate_limit_max`, `rate_limit_window_seconds` | At most N triggers per sliding window per entity; both `0` disables |
| `is_active` | Disabled rules are rejected immediately |

Effect fields: `point_type` (slug; omitted = primary), `direction` (`credit`, the default, or
`debit`), `kind` and its parameters:

| `kind` | Amount | Parameters |
|---|---|---|
| `fixed` | `amount` | `amount` |
| `per_spend` | `floor(spend * rate_points / rate_per)`; nothing for zero or negative spend | `rate_points`, `rate_per`, optional `max` per transaction. Credit only |
| `input` | The execution request's `amount` | Optional `min` / `max`; out-of-range amounts are rejected. At most one per rule |
| `streak` | `steps[day in streak - 1]`, starting over after the last step | `steps`. Claimable once per local day; a missed day restarts the streak. Credit only |

Credits may override the point type's expiry with `expiry_mode` / `expiry_value` (e.g.
`"expiry_mode": "never"` for a lifelong bonus on a type that normally expires).

```sh
curl -X POST localhost:8080/admin/tiers/$TIER_ID/rules -H "Authorization: Bearer $KEY" -d '{
  "slug": "daily_login", "name": "Daily login", "point_delta": 10,
  "cooldown_seconds": 60, "rate_limit_max": 5, "rate_limit_window_seconds": 86400
}'
curl -X POST localhost:8080/admin/tiers/$TIER_ID/rules -H "Authorization: Bearer $KEY" -d '{
  "slug": "order_complete", "name": "Order completed", "order_delta": 1,
  "effects": [
    {"point_type": "coins", "kind": "per_spend", "rate_points": 1, "rate_per": 100, "max": 2000},
    {"point_type": "xp", "kind": "fixed", "amount": 10}
  ]
}'
curl -X POST localhost:8080/admin/tiers/$TIER_ID/rules -H "Authorization: Bearer $KEY" -d '{
  "slug": "redeem", "name": "Redeem at checkout",
  "effects": [{"point_type": "coins", "kind": "input", "direction": "debit", "min": 100, "max": 25000}]
}'
```

### Membership levels

Levels (e.g. Silver, Gold) are ordered by `rank` (higher is better). An entity qualifies for a
level when it meets **all** of the level's requirements within the policy's window:

| Requirement | Compared with |
|---|---|
| `min_spend` | qualifying spend |
| `min_orders` | qualifying orders (`order_delta` of rules, plus adjustment `orders`) |
| `min_points` `{"xp": 500}` | qualifying points earned per point type: rule credits and adjustments, not redemptions or expiry |

Redeeming points therefore never costs an entity its level. Tiers stay rule packages; a level can
optionally link one (`tier_id`), which then applies when an execution omits `tier`.

Spend is sent with executions and adjustments as an integer in the tenant's smallest currency unit
(e.g. paisa or cents). Negative spend is a refund. `min_spend` uses the same unit, and thresholds
are unique per tenant.

Level policy (per tenant):

| Field | Meaning |
|---|---|
| `basis` | `lifetime` (default): all activity counts. `rolling`: only the last `window_days`. `calendar`: fixed periods of `period_months`, aligned to 1 January; the better of the levels met by the current and the previous period applies, so a level reached in one period is kept through the next |
| `window_days` | 1–3650, required for `rolling`. Switching basis clears it |
| `period_months` | 1, 2, 3, 4, 6 or 12, required for `calendar`. Switching basis clears it |
| `downgrade` | `false` (default): levels are never lost. `true`: an entity drops once it stops qualifying and the grace period has passed |
| `grace_days` | 0–3650. How long an entity keeps its level after it stops qualifying |
| `min_tenure_days` | 0–3650. A level can't be lost by a downgrade until it has been held this long |

```sh
curl -X PATCH localhost:8080/admin/level-policy -H "Authorization: Bearer $KEY" \
  -d '{"basis": "rolling", "window_days": 365, "downgrade": true, "grace_days": 30}'
curl -X POST localhost:8080/admin/levels -H "Authorization: Bearer $KEY" \
  -d '{"slug": "silver", "name": "Silver", "rank": 2, "min_spend": 500000}'
curl -X POST localhost:8080/admin/levels -H "Authorization: Bearer $KEY" \
  -d '{"slug": "gold", "name": "Gold", "rank": 3, "min_spend": 100000, "min_orders": 25, "min_points": {"xp": 800}}'
```

How levels are evaluated:

- After every ledger write, in the same transaction. Upgrades apply immediately.
- When an entity stops meeting its level's threshold, it keeps the level and `below_level_since`
  is set (the grace clock). Qualifying again clears it. Even with `downgrade: false` the field is
  set, so tenants can see who is at risk.
- A background sweep (`LEVEL_SWEEP_INTERVAL`, default `1h`, `0` disables) applies time-based
  changes for tenants with a rolling or calendar basis or downgrades enabled: activity ageing out
  of the window, calendar periods ending, and grace periods ending. A Postgres advisory lock keeps
  it to one instance at a time.
- Threshold and policy changes affect an entity at its next evaluation. Call
  `POST /admin/levels/reevaluate` to apply them to everyone at once.
- An entity whose level is deactivated or deleted moves straight to the level it qualifies for,
  regardless of the downgrade policy.

### Execution API

```sh
curl -X POST localhost:8080/engine/execute -H "Authorization: Bearer $KEY" -d '{
  "entity_id": "user123", "tier": "basic", "rule": "daily_login", "idempotency_key": "abc-123"
}'
```

- `spend` (optional, integer, ± for refunds) counts toward the entity's level and drives
  `per_spend` effects.
- `amount` (integer ≥ 1) is required by rules with an `input` effect and rejected by others.
- `tier` is optional when the entity's level links a tier. That tier is used, otherwise
  `422 tier_unresolved`.
- When the transaction moves the entity to another level, the response includes `level_change`
  (`direction`, `from_level`, `to_level`, `qualifying_spend`). It's omitted on idempotent replays.

- The entity can also be passed under the tenant's architecture key, e.g. `{"user_id": "user123", ...}`.
  Numeric IDs are accepted.
- The idempotency key can also be sent as an `Idempotency-Key` header.
- `201` returns the new ledger entry: `transaction_id`, `lines` (one per point type moved:
  `point_type`, `delta`, `balance_before`, `balance_after`), `spend`, `orders`, `amount`, and the
  primary-type summary `point_delta` / `balance_before` / `balance_after`. Streak rules add
  `streak: {"days": n}`.
- `200` with `"replayed": true` (and an `Idempotent-Replayed: true` header) is a retry of an earlier
  request with the same idempotency key. Nothing is applied twice.

| Status | Code | When |
|---|---|---|
| 404 | `tier_not_found`, `rule_not_found` | Unknown tier/rule slug |
| 403 | `tier_inactive`, `rule_inactive` | Tier or rule toggled off |
| 429 | `cooldown_active`, `rate_limit_exceeded` | Limit hit; `Retry-After` header gives seconds to wait |
| 422 | `insufficient_balance` | A deduction would take a point type's balance below zero |
| 422 | `point_limit_exceeded` | A debit would exceed the point type's daily/weekly/monthly cap |
| 422 | `amount_required`, `amount_not_accepted`, `amount_out_of_range` | `amount` missing for an input rule, given to a rule without one, or outside its `min`/`max` |
| 429 | `streak_already_claimed` | The streak rule was already claimed today; `Retry-After` is the time to local midnight |
| 422 | `spend_below_zero`, `orders_below_zero` | A refund or cancellation exceeds the entity's recorded spend or orders |
| 422 | `tier_unresolved` | No `tier` given and the entity's level has no linked tier |
| 422 | `idempotency_key_reused` | Same key, different request |
| 409 | `idempotency_in_progress` | Same key is being processed right now; retry |
| 409 | `architecture_not_configured` | Architecture key not set yet |
| 503 | `limiter_unavailable` | Redis unreachable. Requests fail closed rather than skip limits |

### Query & log API

| Method & path | Purpose |
|---|---|
| `GET /data/profiles/{entity_id}` | Balances per point type, current tier, spend, orders and level of one entity |
| `GET /data/profiles/{entity_id}/expirations` | Points due to expire, soonest first: `point_type`, `amount`, `expires_at` |
| `GET /data/profiles?tier=&level=&limit=&cursor=` | All profiles, ordered by entity ID |
| `GET /data/profiles/{entity_id}/level-history?limit=&cursor=&order=` | One entity's level changes |
| `GET /data/profiles/{entity_id}/logs?point_type=&limit=&cursor=&order=` | One entity's ledger |
| `GET /data/logs?from=&to=&tier=&rule=&entity_id=&entry_type=&point_type=&limit=&cursor=&order=` | Tenant-wide ledger. `entry_type` is `rule`, `adjustment` or `expiry` |

Lists return `{"data": [...], "next_cursor": "..." | null}`; pass `next_cursor` back as `cursor`.
`limit` defaults to 50 (max 500); `order` is `desc` (newest first, default) or `asc`. `from`/`to`
are RFC 3339 timestamps (`from` inclusive, `to` exclusive).

## Design notes

- **Atomicity and concurrency.** Each transaction runs as one Postgres transaction that ensures
  the profile exists and locks its row (serializing concurrent writes to the same entity), expires
  due lots, computes every effect, updates each point type's balance and lots, and inserts the
  ledger entry with its lines. `CHECK (balance >= 0)` on every balance rejects deductions an entity
  can't afford, and per-period caps are summed from the ledger under the same lock.
- **Limits.** Cooldowns and sliding-window throttles are checked and reserved in one atomic Redis
  Lua script, using Redis server time so every instance shares one clock. If the database write
  then fails, the reservation is released, so failed requests don't consume quota. Keys use hash
  tags and work on Redis Cluster.
- **Idempotency.** A short Redis lock keeps concurrent duplicates from racing. The durable record
  is a unique `(tenant_id, idempotency_key)` index on the ledger, so a key is honoured indefinitely.
- **Append-only ledger.** Triggers reject `UPDATE`, `DELETE` and `TRUNCATE` on `ledger_entries`
  and `ledger_lines`.
  Corrections go through `POST /admin/adjustments`. Ledger rows store tier/rule slugs, so history
  survives configuration changes and deletions.
- **Tenant isolation.** Every query filters on `tenant_id`, and rules reference tiers through a
  composite `(tenant_id, tier_id)` foreign key, so a row can't point at another tenant's data.
- **Statelessness.** Instances keep no local state. All state lives in Postgres and Redis.
- **Current tier.** A profile's tier is the tier of its most recent rule execution. Adjustments
  don't change it. Automatic progression is handled by levels (above), not by tiers.
- **Throughput.** A single instance on a developer laptop sustained ~5,500 writes/s at p95 ≈ 12 ms
  (10k requests, 50 concurrent clients, every request a full balance update + ledger write).
