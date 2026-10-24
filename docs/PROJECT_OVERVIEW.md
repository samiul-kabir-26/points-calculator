# Points Calculator: Project Overview

This document explains what the project is, how its pieces fit together, and how to configure a
loyalty program with it. For endpoint-by-endpoint reference, see the [README](../README.md). For
the original brief, see [point_calculation_engine_requirements.md](point_calculation_engine_requirements.md).
For multiple point types, expiry, caps, streaks and multi-criteria levels, see
[MULTI_CURRENCY_DESIGN.md](MULTI_CURRENCY_DESIGN.md). [tenants/](tenants/) has complete worked
configurations modelled on Daraz, Shopee and Lazada.

## 1. What it is

A **multi-tenant loyalty engine**. Each business that uses it (a *tenant*, e.g. an e-commerce shop)
configures its own program through an admin API or the built-in console. Its backend then reports
events, such as "user 42 placed an order worth ৳1,250", to one execution endpoint. The engine:

- awards or deducts **points** of one or more **point types** (e.g. spendable coins and
  non-spendable XP) according to the tenant's rules, including amounts proportional to spend,
  caller-supplied amounts and daily-streak rewards,
- expires earned points on schedule and caps how much can be earned or redeemed per period,
- enforces cooldowns, rate limits and idempotency so points can't be farmed or double-credited,
- tracks **spend**, **orders** and **points earned**, and moves users between **membership
  levels** (Basic, Silver, Gold, ...),
- records every change in an **append-only ledger** that can be audited later.

It is a Go service backed by PostgreSQL (state) and Redis (limits and locks). Instances are
stateless and can be scaled horizontally.

## 2. Core concepts

| Concept | Meaning |
|---|---|
| **Tenant** | A business using the engine. All data is isolated per tenant. |
| **API key** | How a tenant authenticates (`pce_...`). Shown once, stored as a SHA-256 hash, rotatable. |
| **Architecture key** | The name of the field the tenant identifies users by, e.g. `user_id` or `wallet_address`. Locked once profiles exist. |
| **Entity** | One user (or wallet, device, ...) identified by that key. |
| **Point type** | A currency the tenant defines: `coins`, `gems`, `xp`, ... Spendable or not, with an expiry policy and per-period caps. One is **primary**; legacy single-balance fields refer to it. |
| **Tier** | A **rule package**, e.g. "Basic", "Gold perks" or "Winter promo". Groups rules and can be switched on or off as a whole. |
| **Rule** | One action inside a tier: a list of **effects** (what it does to each point type), an `order_delta`, plus an optional cooldown and rate limit. |
| **Effect** | `fixed`, `per_spend` (points per unit of spend), `input` (amount supplied by the caller) or `streak` (escalating daily reward), as a credit or a debit. |
| **Profile** | An entity's current state: a balance per point type, spend, orders, level. Created on the entity's first transaction. |
| **Lot** | The part of a balance that expires on a given date. Redemptions use the soonest-expiring lot first. |
| **Ledger entry** | One immutable record of a transaction (`rule`, `adjustment` or `expiry`), with one **line** per point type moved, plus spend, orders, rule and reason. |
| **Adjustment** | A manual compensating entry (the ledger can't be edited). It can change one point type, spend, orders, or any combination. |
| **Level** | A membership status (Silver, Gold, ...) with a rank, earned by meeting its spend, order and points requirements. Can optionally link a tier. |
| **Level policy** | A tenant's rule for turning activity into a level: lifetime, rolling or calendar-period window, downgrades, grace period, minimum tenure. |

### Tiers and levels are different things

This is the most important design decision in the project:

- A **tier** answers *"which rules apply to this event?"* The caller can name it explicitly.
- A **level** answers *"how valuable is this customer?"* The engine computes it from spend.

They connect through an optional link: a level can point at a tier. When an execution leaves out
`tier`, the engine uses the tier linked to the user's level. So "Gold members earn 2× on orders"
is configured by linking the Gold level to a tier whose `order_complete` rule awards double points.

Keeping them apart means a tenant can also have tiers that aren't levels (a paid "Premium"
subscription, a seasonal promo), or levels that don't change earning at all (status only).

## 3. Points vs. spend: the numbers on a profile

| Field | Unit | Meaning |
|---|---|---|
| `balances.<type>.balance` | points | The entity's holding of each point type. Never below zero. |
| `balances.<type>.lifetime_earned` | points | Rule credits plus adjustments. Redemptions and expiry don't reduce it. |
| `balances.<type>.qualifying_earned` | points | `lifetime_earned` within the level window; what `min_points` is compared with. |
| `balance` | points | The primary type's balance, kept for backward compatibility. |
| `lifetime_spend` | money | Total order value ever reported, minus refunds. Never resets. |
| `qualifying_spend` | money | The spend that counts toward the level under the tenant's policy: everything (lifetime), the last *N* days (rolling), or the current calendar period (calendar). |
| `lifetime_orders`, `qualifying_orders` | orders | The same two views of the order count (rules' `order_delta`, plus adjustments). |

Money is an integer in the tenant's **smallest currency unit** (paisa, cents): `500000` = ৳5,000.00.
This avoids floating-point rounding. Each tenant is assumed to use one currency.

**Points and spend are independent.** Redeeming points never touches spend, so **redeeming never
costs a user their level**. This matches how most loyalty programs work (airlines, Sephora,
marketplace VIP programs). A refund is the opposite: the caller sends negative spend, which lowers
qualifying spend and can eventually cost the user their level.

A typical "XP + coins" program uses a non-spendable `xp` type for status and a spendable,
expiring `coins` type for rewards. Levels can then require XP, spend and orders together.

## 4. Architecture

```
            ┌──────────────────────┐        ┌───────────────────────────┐
 Tenant ───▶│  API instance (Go)   │───────▶│ PostgreSQL                │
 backend    │  • admin API         │        │ tenants, tiers, rules,    │
            │  • /engine/execute   │        │ levels, profiles, ledger, │
 Admin  ───▶│  • /data queries     │        │ level history             │
 console    │  • embedded UI       │        └───────────────────────────┘
            │  • level sweep timer │        ┌───────────────────────────┐
            └──────────────────────┘───────▶│ Redis                     │
                 (N stateless copies)       │ cooldowns, rate limits,   │
                                            │ idempotency locks         │
                                            └───────────────────────────┘
```

- **PostgreSQL** is the source of truth. Balances, spend and levels change only inside database
  transactions.
- **Redis** holds short-lived counters. If Redis is down, executions **fail closed** (`503`) rather
  than skip limits.
- The **level sweep** runs inside each API process on a timer. A Postgres advisory lock ensures
  only one instance does the work at a time.

### Code layout

```
cmd/
  api/            HTTP service entry point; starts the server and the level sweep
  seed/           Applies a JSON program (tiers, rules, levels, policy) to a tenant
internal/
  api/            HTTP handlers, validation, auth middleware, embedded admin UI (ui/index.html)
  engine/         Execution logic: tier resolution, limits, idempotency, atomic apply
  store/          All SQL; every query is scoped by tenant_id
    levels.go     Level CRUD, policy, level evaluation, sweep, level history
  limiter/        Redis Lua scripts for cooldowns, sliding-window throttles, locks
  database/       Connection pool and embedded migrations (migrations/*.sql)
  config/         Environment variable loading
  apperr/         Typed errors that map to HTTP status + error code
seeds/            Sample loyalty program (loyalty_program.json)
docs/             Requirements and this overview
```

## 5. Life of an execution request

`POST /engine/execute` with
`{"user_id": "42", "rule": "order_complete", "spend": 125000, "idempotency_key": "order-9001"}`:

1. **Authenticate.** The API key's hash is looked up and the tenant (with its level policy) is loaded.
2. **Validate the body.** The entity can be given as `entity_id` or under the architecture key name.
   Unknown fields are rejected.
3. **Idempotency.** If the key was already used for the same request, the original result is
   returned (`200`, `replayed: true`). The same key with a different request gets `422`. A short
   Redis lock stops concurrent duplicates.
4. **Resolve the tier.** Use `tier` if given. Otherwise use the tier linked to the user's level; a
   user with no level yet is treated as being on the level their spend qualifies for, so a
   zero-threshold base level applies from the first event.
5. **Check the rule.** The tier and rule must exist and both be active.
6. **Reserve limits.** One atomic Redis script checks the cooldown and sliding-window cap and
   reserves a slot, using Redis server time so all instances share a clock.
7. **Apply atomically.** In one Postgres transaction:
   - create the profile if needed and lock it (`lifetime_spend += spend`,
     `lifetime_orders += order_delta`), so concurrent events for one user apply one after another,
   - expire any of the user's lots that are already due (an `expiry` entry),
   - advance the streak for streak rules,
   - compute each effect (fixed, per spend, input, streak), clamp credits to the point type's
     period caps and refuse debits that would exceed them,
   - update each point type's balance; credits that expire go into a lot, debits use the
     soonest-expiring lots first,
   - insert the ledger entry with one line per point type,
   - **re-evaluate the level** (section 6).

   `CHECK` constraints reject a negative balance (`insufficient_balance`) or a refund larger than
   recorded spend or orders (`spend_below_zero`, `orders_below_zero`).
8. **On failure,** the limiter reservation is released, so failed requests don't use up quota.
9. **Respond `201`** with the ledger entry, plus `level_change` if the user moved level.

Adjustments (`POST /admin/adjustments`) follow steps 1–3 and 7 without rules or limits.

## 6. How levels are evaluated

Evaluation happens after **every ledger write** (same transaction), when the **sweep** runs, and on
**manual re-evaluation**.

1. Compute qualifying activity (spend, orders, points earned per type): lifetime totals, the
   last `window_days` (rolling), or the current calendar period (calendar). Each figure is
   clamped at zero.
2. Find the **target**: the highest-ranked active level whose requirements are all met. Under the
   calendar basis, the previous period is checked too and the better target wins.
3. Decide:
   - **Target is the same as or above the current level:** move to it immediately. Clear the grace clock.
   - **Target is below the current level:** keep the current level and start the grace clock
     (`below_level_since`) if it isn't running. If `downgrade` is on, `grace_days` have passed and
     the level has been held for `min_tenure_days`, move down to the target.
   - **Current level was deactivated or deleted:** move straight to the target.
4. Every move is recorded in `level_changes` (direction, from, to, qualifying spend).

The grace clock runs even when downgrades are off, so the console can flag users as **at risk**.

**Why a sweep is needed:** with a rolling window, a user who does nothing still loses qualifying
spend as old orders age out, and grace periods end with no event to trigger an evaluation. The
sweep (`LEVEL_SWEEP_INTERVAL`, default hourly) re-evaluates users who have a level or non-zero
qualifying spend, for tenants whose policy is rolling or allows downgrades. Profiles are written
only when something changed.

### Level policy options

| Field | Values | Effect |
|---|---|---|
| `basis` | `lifetime` (default) / `rolling` / `calendar` | Whether all activity counts, only recent activity, or activity in fixed calendar periods |
| `window_days` | 1–3650 | Rolling window length (rolling only) |
| `period_months` | 1, 2, 3, 4, 6, 12 | Calendar period length, aligned to 1 January (calendar only) |
| `downgrade` | `false` (default) / `true` | Whether a level can ever be lost |
| `grace_days` | 0–3650 | How long a user keeps a level after they stop qualifying |
| `min_tenure_days` | 0–3650 | Minimum time a level is held before it can be lost |

### Common recipes

| Program style | Policy |
|---|---|
| "Once Gold, always Gold" | `lifetime`, `downgrade: false` |
| Airline-style yearly status | `rolling`, `window_days: 365`, `downgrade: true`, `grace_days: 30–90` |
| Strict monthly VIP | `rolling`, `window_days: 30`, `downgrade: true`, `grace_days: 0` |
| Status that only refunds can remove | `lifetime`, `downgrade: true` |
| Shopee-style half-years | `calendar`, `period_months: 6`, `downgrade: true` |
| Membership kept for at least 6 months | `rolling`, `downgrade: true`, `min_tenure_days: 180` |

## 7. Data model

| Table | Purpose |
|---|---|
| `tenants` | Name, architecture key, time zone, level policy columns |
| `point_types` | Currencies per tenant: primary flag, spendable, expiry policy, caps |
| `api_keys` | Hashed keys with prefix and revocation time |
| `tiers` | Rule packages per tenant, unique slug |
| `rules` | Order delta and limits, belongs to a tier |
| `rule_effects` | A rule's effect on one point type (kind, direction, amounts, rate, steps, expiry override) |
| `levels` | Slug, `rank` (unique per tenant), `min_spend`, `min_orders`, optional `tier_id`, active flag |
| `level_requirements` | A level's minimum points earned per point type |
| `entity_profiles` | Last tier, lifetime and qualifying spend and orders, current level, `level_since`, `below_level_since` |
| `entity_balances` | Balance, lifetime earned and qualifying earned per entity and point type |
| `point_lots` | Expiring part of a balance, one row per expiry instant (working state, deleted when empty) |
| `entity_streaks` | Current streak length and last claim day per entity and rule slug |
| `ledger_entries` | Append-only transaction log (`rule`, `adjustment`, `expiry`; spend, orders, input amount, rule/tier slugs, idempotency key, reason, primary-type summary) |
| `ledger_lines` | Append-only per-point-type movements of each entry, with balances before and after |
| `level_changes` | Append-style history of level moves, with the spend, orders and points behind them |

Integrity rules enforced by the database:

- **Tenant isolation:** child rows reference parents through composite `(tenant_id, id)` foreign
  keys, so a rule can't point at another tenant's tier and a profile can't hold another tenant's level.
- **Immutable ledger:** triggers reject `UPDATE`, `DELETE` and `TRUNCATE`. Tier, rule and level
  names are copied onto history rows, so history survives deletions.
- **Non-negative balances, lifetime spend and lifetime orders** via `CHECK` constraints.
- **Point types in use can't be deleted:** balances, effects, level requirements and ledger lines
  reference them with `RESTRICT`.
- **Idempotency:** a unique `(tenant_id, idempotency_key)` index makes keys permanent.

Note: the profile's `tier` is simply the tier of the user's most recent rule execution. It is
informational; the level is the user's status.

## 8. Configuring a program: worked example

`seeds/loyalty_program.json` is a complete sample. Apply it with
`go run ./cmd/seed -key pce_... -file seeds/loyalty_program.json`. It is idempotent.

**Tiers (rule packages):** `basic`, `silver`, `gold`, `platinum` with increasing earn rates;
`premium` (paid subscription, named explicitly by the caller); `winter_bonus` (seasonal, inactive
until switched on).

**Levels:**

| Level | `min_spend` | Linked tier |
|---|---|---|
| Basic | 0 | basic |
| Silver | 500,000 (৳5,000) | silver |
| Gold | 2,000,000 (৳20,000) | gold |
| Platinum | 5,000,000 (৳50,000) | platinum |

**Policy:** rolling 365 days, downgrades on, 30-day grace.

What happens to sample users:

| Scenario | Result |
|---|---|
| New user logs in | Basic level; earns Basic login points |
| Two ৳3,000 orders | Silver after the second; later orders earn Silver rates |
| Silver user redeems a voucher | Points drop, level stays Silver |
| One ৳25,000 order | Straight to Gold; that order earns at the old rate, Gold rates apply from the next one |
| Gold user refunds ৳10,000 | Still Gold but **at risk**; drops to Silver after 30 days unless they spend again |
| Gold user stops buying for a year | Spend ages out of the window, grace starts, then they drop to the level their recent spend earns |

**Integration from the tenant's backend:**

```sh
# Order placed: award points and count spend. tier omitted, so the user's level decides.
curl -X POST $API/engine/execute -H "Authorization: Bearer $KEY" \
  -d '{"user_id": "42", "rule": "order_complete", "spend": 125000, "idempotency_key": "order-9001"}'

# Order refunded: claw back points and spend.
curl -X POST $API/admin/adjustments -H "Authorization: Bearer $KEY" \
  -d '{"entity_id": "42", "points": -50, "spend": -125000, "reason": "Order 9001 refunded"}'

# Show the user their status.
curl $API/data/profiles/42 -H "Authorization: Bearer $KEY"
```

Use the order or event ID as the idempotency key, so network retries never double-credit.

## 9. Admin console

Served at `/ui/`: a single HTML file with no build step that calls the same public API.

- **Tiers & rules:** create and edit, on/off toggles, ready-made API snippets per rule.
- **Levels:** level policy form, levels table with thresholds and linked tiers, "Re-evaluate all".
- **Profiles:** balance, level, qualifying spend, at-risk badge, per-user level history.
- **Ledger:** filterable audit trail showing both points delta and spend.
- **Test & adjust:** fire test executions (with spend, or with the tier taken from the level) and
  record adjustments.
- **Settings:** architecture key, API endpoint summary, API key rotation.

New tenants are onboarded from the login screen with the platform admin token.

## 10. Running and operating

```sh
PLATFORM_ADMIN_TOKEN=change-me make up   # everything in Docker, API on :8080
make test                                # unit tests
make test-integration                    # full HTTP stack against real Postgres/Redis, with -race
```

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:8080` | Listen address |
| `DATABASE_URL` | required | Postgres connection string (PostgreSQL 15+) |
| `REDIS_URL` | `redis://localhost:6379/0` | Redis for limits and locks |
| `PLATFORM_ADMIN_TOKEN` | required | Token for onboarding tenants |
| `AUTO_MIGRATE` | `true` | Apply embedded migrations on startup (advisory-locked) |
| `DB_MAX_CONNS` | `20` | Pool size |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `SHUTDOWN_TIMEOUT` | `15s` | Graceful shutdown window |
| `LEVEL_SWEEP_INTERVAL` | `1h` | Time-based level evaluation; `0` disables |

The production image is a static binary on a distroless, non-root base.

## 11. Known limitations and possible next steps

- **Explicit tiers aren't checked against levels.** A caller that sends `"tier": "gold"` gets Gold
  rules even for a Basic user. Enforcement could be added as an opt-in policy flag.
- **Re-evaluate all runs while the request waits.** That's fine for modest tenants; a very large
  tenant would need it as a background job.
- **Single money currency per tenant.** Several *point* types are supported, but spend is one
  currency; multi-currency spend would need a currency per spend entry and conversion rules.
- **Per-order redemption caps as a share of the order** (e.g. "at most 5% of the order") and
  excluded product categories stay in the tenant's checkout code.
- **Rate-limited orders are rejected outright,** so their spend and order count aren't recorded.
  A tenant can record them through a rule with no point effects.
- **`level_change` isn't included in idempotent replays.** The level history endpoint is the
  durable record.
- **No outbound notifications yet.** Webhooks on level changes ("You're Gold now!") are a natural
  next feature, since every change is already recorded.
- **Throughput not re-measured since levels were added.** The earlier figure was about 5,500
  writes/s on a laptop; level evaluation adds two or three small queries per write.
