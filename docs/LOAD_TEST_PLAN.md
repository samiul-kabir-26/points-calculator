# Load Test Plan

How to measure the throughput, latency and limits of the Points Engine, and find where it breaks
first.

## 1. Goals and targets

The requirements document asks for "thousands of requests per second" and **p95 < 50 ms** on
executions. The README quotes ~5,500 writes/s at p95 ≈ 12 ms, measured **before** levels and
multiple point types existed. Each execution now does more database work, so that figure needs
re-measuring.

| Question | How we answer it |
|---|---|
| What is sustained throughput for one instance? | Ramp scenario A until p95 crosses 50 ms or errors appear |
| Does it meet p95 < 50 ms at the expected production load? | Scenario B (realistic mix) at the target rate for 30 min |
| How does it scale with more instances? | Repeat B with 1, 2 and 4 instances behind a load balancer |
| What breaks first: API CPU, Postgres, the connection pool, or Redis? | Resource metrics during the stress run (section 5) |
| Do the correctness guarantees hold under load? | Post-run checks (section 7): no double credits, balanced ledger, caps respected |
| Is it stable over time? | 2-hour soak |

**Pass criteria (proposed; adjust to real traffic estimates):**

| Metric | Target |
|---|---|
| `POST /engine/execute` p95 / p99 | < 50 ms / < 150 ms at target load |
| `GET /data/profiles/{id}` p95 | < 20 ms |
| Unexpected errors (5xx, timeouts) | < 0.1% |
| Expected business rejections (429/422) | Counted separately, never treated as failures |
| Correctness checks | 100% pass |

## 2. What every execution costs today

This shows where load will concentrate. Nothing is cached in the application; Redis holds only
limiter state.

| Step | Store | Round trips |
|---|---|---|
| Authenticate API key → tenant | Postgres | 1 |
| Resolve tier from level (when `tier` is omitted) | Postgres | 1 |
| Load rule + tier, then its effects | Postgres | 2 |
| Idempotency: lock, look up previous result, unlock | Redis 2 + Postgres 1 | 3 |
| Cooldown / rate-limit reservation (Lua script) | Redis | 1 (0 if the rule has no limits) |
| **Transaction:** create/lock profile, load point types, expire due lots, streak, per-debit balance check, per-cap sum, per point type balance upsert + lot write, insert entry + lines, load level ladder + requirements, read profile + balances, window sums (0 lifetime / 2 rolling / 4 calendar), cache updates | Postgres | ~15-25 |

Things that grow over time and need specific tests:
- **Window sums** (rolling/calendar levels, period caps) scan an entity's ledger rows in the
  window. Entities with long histories cost more per transaction.
- **One entity is serialized** by its profile row lock. A single very busy entity (a bot, a
  shared account) queues up behind itself.
- **Pool size:** `DB_MAX_CONNS` defaults to 20. Each execution holds a connection for the whole
  transaction.

## 3. Environment

| Item | Setup |
|---|---|
| Hardware | Dedicated machines, not a developer laptop: one or more API hosts, one Postgres host, one Redis host. Record CPU, RAM and disk type |
| Isolation | A separate database (never the dev `points` DB). The load generator runs on its own machine on the same network |
| Build | `make build`; production settings: `LOG_LEVEL=warn` (request logging is per request), `AUTO_MIGRATE=true` once, then off |
| Postgres | 16, with `pg_stat_statements` enabled; record `shared_buffers`, `max_connections`, `synchronous_commit` |
| Redis | 7, no persistence tuning changes from production |
| Sweeps | Left on (`LEVEL_SWEEP_INTERVAL`, `EXPIRY_SWEEP_INTERVAL`) except in scenario F, where they're driven deliberately |
| Load balancer (scaling runs) | nginx or HAProxy, round robin, keep-alive to the backends |
| Load tool | [k6](https://k6.io), with its thresholds and per-scenario tags; output to Prometheus or JSON |

## 4. Test data

Create with the seed tool plus a data-generation script (not committed with secrets):

| Data | Volume | Why |
|---|---|---|
| Tenants | The 3 seeded ones (Daraz, Shopee, Lazada) + 1 plain tenant (lifetime levels, one point type) | Covers every level basis, expiry mode and effect kind |
| Entities per tenant | 100,000 existing profiles + new ones created during the run (~10% of traffic) | Realistic index sizes; first transactions follow a different path |
| Ledger history | Two cohorts: "light" (≤ 20 entries) and "heavy" (2,000+ entries in the last 180 days) | Measures how window sums scale |
| Open lots | Daraz/Lazada entities with up to 90/180 lots; a cohort with lots already due | Debit lot consumption and in-transaction expiry |
| Idempotency keys | Unique per request except in scenario D | Unique-index growth |

Generate history by replaying executions through the API (correct by construction), at least
once before the first run. Snapshot the database afterwards so every run starts from the same
state (`pg_dump -Fc` / restore, or a disk snapshot).

## 5. Metrics to collect

| Layer | Metrics |
|---|---|
| k6 | Requests/s, p50/p95/p99 latency per scenario and endpoint, status code breakdown, dropped iterations |
| API host | CPU, memory, goroutines (add `net/http/pprof` on a private port for profiling runs), GC pauses |
| Postgres | TPS, active vs idle connections, lock waits (`pg_stat_activity` wait events, `pg_locks`), top queries by total time (`pg_stat_statements`), buffer hit ratio, checkpoints, WAL rate, disk IOPS and latency |
| Redis | ops/s, latency (`--latency`), CPU, memory, connected clients |
| Engine | `limiter_unavailable` (503) count, sweep durations from logs ("level sweep finished", "expiry sweep finished") |

## 6. Scenarios

Every scenario tags requests so results can be separated, and treats `422`, `429` and replayed
`200`s as expected outcomes where noted.

### A. Baseline throughput: one simple rule
- Plain tenant, a `fixed` earn rule with no limits, random entity from 100k, unique idempotency
  key.
- **Ramp:** 100 → 5,000 req/s in steps of 250 every 2 minutes (k6 `ramping-arrival-rate`).
- **Finds:** the single-instance ceiling and the knee in the latency curve. Compare with the old
  5,500/s figure.

### B. Realistic mix (the main test)
Per tenant, roughly matching a marketplace's traffic:

| Share | Call |
|---|---|
| 35% | `order_complete` with spend (per-spend effect, rate limit, level evaluation) |
| 20% | `daily_check_in` (streak; repeats on the same day return 429, which is expected) |
| 10% | `redeem_at_checkout` with an amount (lot consumption, period caps; some 422s expected) |
| 5% | games/missions with an input amount |
| 2% | `order_cancelled` with negative spend |
| 25% | `GET /data/profiles/{id}` and `/expirations` |
| 3% | `GET /data/profiles/{id}/logs` |

- **Load:** a constant arrival rate at the target (e.g. 1,000 req/s), 30 minutes, after a
  5-minute warm-up.
- **Variants:** all three tenants at once; heavy-history cohort only.

### C. Hot entity contention
- 200 virtual users hitting **one** entity with an earn rule, plus a background of scenario B.
- **Expect:** writes to that entity serialize; their latency rises while other entities stay
  unaffected. **Watch:** lock waits, and whether pool exhaustion spreads the slowdown to other
  entities.

### D. Idempotency storm
- 20% of requests reuse a key from the last 10 seconds (client retries), including concurrent
  duplicates.
- **Expect:** exactly one `201` per key; the rest `200 replayed` or `409 idempotency_in_progress`.
  No double credits (checked in section 7).

### E. Rejection-heavy traffic
- Abuse pattern: a rule with a cooldown hammered by the same entities, so 90%+ of requests get 429.
- **Expect:** rejections are cheap (Redis only, no Postgres transaction). Verify that p95 for 429s
  is well below that of successful writes.

### F. Sweeps under load
- Prepare 1M lots due now spread over 200k entities. Run scenario B at the target rate and
  trigger the expiry sweep (restart with `EXPIRY_SWEEP_INTERVAL=1m`).
- Same with the level sweep on a calendar tenant at a period boundary (simulate by setting
  `period_months: 1` and running near month end, or with many rolling-window entities).
- **Watch:** foreground p95 during the sweep, sweep duration, and lock waits (the sweep locks one
  entity at a time).

### G. Read path and pagination
- `GET /data/logs` with filters and cursors over a tenant with 50M ledger rows; profile list
  pagination.
- **Finds:** slow queries and missing indexes for the `point_type` filter and date ranges.

### H. Horizontal scaling
- Repeat B at increasing rates with 1, 2 and 4 API instances behind the load balancer. Raise
  `DB_MAX_CONNS` only within what Postgres can sustain (total connections = instances ×
  pool size).
- **Expect:** near-linear scaling until Postgres becomes the bottleneck; record where.

### I. Soak
- Scenario B at 70% of the measured ceiling for 2 hours.
- **Watch:** memory growth, connection leaks, Redis memory (rate-limit keys expire by TTL),
  table and index bloat, autovacuum activity, steady latency.

### J. Spike and failure
- **Spike:** jump from 20% to 150% of the ceiling for 60 seconds, then back. Check recovery time
  and that no requests hang past the server's 30 s write timeout.
- **Redis outage:** stop Redis for 30 seconds during B. Expect `503 limiter_unavailable` (fail
  closed), no corrupted balances, and recovery without a restart.
- **Postgres failover or restart** (if applicable): expect errors during the outage and recovery
  afterwards.

## 7. Correctness checks after every run

Run these against the load-test database once traffic stops:

1. **Ledger chain per entity and point type:** every line's `balance_before` equals the previous
   line's `balance_after`, and the last `balance_after` equals `entity_balances.balance`.
2. **No negative balances, and lots never exceed the balance:**
   `sum(point_lots.remaining) <= entity_balances.balance` for every entity and type.
3. **Idempotency:** at most one ledger entry per `(tenant_id, idempotency_key)` (enforced by a
   unique index; confirm no 5xx came from it).
4. **Caps respected:** for each capped point type, no entity's rule debits in any day/week/month
   exceed the cap.
5. **Rate limits respected:** for rules with `rate_limit_max`, no entity exceeds it in any window
   (from ledger timestamps).
6. **Levels consistent:** re-running `POST /admin/levels/reevaluate` changes nothing (zero
   `changed`) unless time-based transitions were due.
7. **Totals reconcile:** the sum of successful `201` deltas the load tool recorded equals the sum
   of ledger deltas for the test window.

## 8. Expected bottlenecks and what to try

Ordered by likelihood. Confirm each with data before changing code.

| Hypothesis | Evidence to look for | Possible fix |
|---|---|---|
| Per-request tenant, rule and effects lookups dominate simple executions | `pg_stat_statements` top calls are `GetTenantByAPIKeyHash`, `GetExecutableRule`, the effects query | Short-TTL in-process cache (e.g. 5-30 s) for tenants, rules and effects, invalidated on admin writes |
| Loading the level ladder and point types in every transaction | Same, for the `levels` / `level_requirements` / `point_types` queries | Cache per tenant with a version counter bumped by admin changes |
| Window sums slow for heavy-history entities | Latency gap between light and heavy cohorts; sequential scans | Per-day aggregate table (spend, orders, earned per type), maintained in the transaction |
| Connection pool exhaustion | Requests waiting on the pool while Postgres CPU is low | Raise `DB_MAX_CONNS`, or put PgBouncer (transaction mode) in front |
| Postgres write/WAL throughput | High WAL rate, commit latency, disk IOPS saturated | Faster disk; consider `synchronous_commit` trade-offs only with explicit sign-off |
| Request logging cost | API CPU in `slog` at high rates | `LOG_LEVEL=warn`, or sampled access logs |
| Hot entity serialization | Scenario C | Expected by design (correctness); add a per-entity rate limit for abuse |

## 9. Execution schedule

| Step | Work | Time |
|---|---|---|
| 1 | Provision the environment, enable `pg_stat_statements`, add pprof behind a flag | ½ day |
| 2 | Data generation and snapshot | ½-1 day |
| 3 | k6 scripts for scenarios A-J, with thresholds | 1 day |
| 4 | Runs A, B, C, D, E (single instance) | ½ day |
| 5 | Runs F, G, H, I, J | 1 day |
| 6 | Correctness checks, analysis, report | ½ day |
| 7 | Fix the top bottleneck, re-run A and B, compare | as needed |

## 10. Report template

For each scenario: configuration (instances, pool size, hardware), load shape, requests/s
achieved, p50/p95/p99 per endpoint, status breakdown, peak CPU/memory per tier, top 5 queries by
total time, correctness check results, and a one-line conclusion. End with the measured ceiling
per instance, the first bottleneck, and the recommended production sizing.

## Appendix: k6 skeleton for scenario B

```js
import http from "k6/http";
import { check } from "k6";
import { uuidv4 } from "https://jslib.k6.io/k6-utils/1.4.0/index.js";

const API = __ENV.API;            // e.g. http://loadtest-lb:8080
const KEY = __ENV.DARAZ_KEY;
const headers = { Authorization: `Bearer ${KEY}`, "Content-Type": "application/json" };

export const options = {
  scenarios: {
    mix: { executor: "constant-arrival-rate", rate: 1000, timeUnit: "1s", duration: "30m",
           preAllocatedVUs: 200, maxVUs: 2000 },
  },
  thresholds: {
    "http_req_duration{kind:write}": ["p(95)<50", "p(99)<150"],
    "http_req_duration{kind:read}": ["p(95)<20"],
    // responseCallback below marks 422/429 (and 404 on reads) as expected, so only real failures count.
    "http_req_failed": ["rate<0.001"],
  },
};

const customer = () => `lt_${Math.floor(Math.random() * 100000)}`;
// 422/429 and replays are business outcomes, not failures.
const expected = http.expectedStatuses(200, 201, 422, 429);

export default function () {
  const r = Math.random();
  let res;
  if (r < 0.35) {
    res = http.post(`${API}/engine/execute`, JSON.stringify({
      customer_id: customer(), rule: "order_complete",
      spend: 10000 + Math.floor(Math.random() * 400000), idempotency_key: uuidv4(),
    }), { headers, tags: { kind: "write" }, responseCallback: expected });
  } else if (r < 0.55) {
    res = http.post(`${API}/engine/execute`, JSON.stringify({ customer_id: customer(), rule: "daily_check_in" }),
      { headers, tags: { kind: "write" }, responseCallback: expected });
  } else if (r < 0.65) {
    res = http.post(`${API}/engine/execute`, JSON.stringify({
      customer_id: customer(), rule: "redeem_at_checkout", amount: 100 + Math.floor(Math.random() * 2000),
      idempotency_key: uuidv4(),
    }), { headers, tags: { kind: "write" }, responseCallback: expected });
  } else {
    res = http.get(`${API}/data/profiles/${customer()}`,
      { headers, tags: { kind: "read" }, responseCallback: http.expectedStatuses(200, 404) });
  }
  check(res, { "no 5xx": (x) => x.status < 500 });
}
```
