# Shopee on the Points Engine

How Shopee's loyalty program works, how it is configured as a tenant in this engine
(`seeds/shopee.json`), and how Shopee's backend talks to the engine.

> **Sources and assumptions.** This follows Shopee Singapore's help-centre pages for Shopee
> Coins and the Shopee Members Club terms, plus Malaysian and Philippine coverage for tier
> thresholds. Shopee doesn't publish every number (Platinum's Singapore threshold, coin caps,
> check-in rewards), so values marked *illustrative* are stand-ins. Everything is configuration.

## 1. How Shopee's points system works

Shopee runs **two separate tracks**:

1. **Shopee Coins**, a spendable currency that expires at the end of the third month.
2. **Shopee Members Club**, a status ladder (Classic → Silver → Gold → Platinum) earned from
   **completed orders and spend** within a fixed half-year.

```mermaid
flowchart LR
    subgraph coins["Shopee Coins (spendable)"]
        direction TB
        E1["Completed orders"] --> W(("Coins<br/>expire at the end of the<br/>3rd month after earning"))
        E2["Daily check-in"] --> W
        E3["Shopee Games"] --> W
        E4["Coins-cashback vouchers"] --> W
        W --> R["100 coins = S$1 off<br/>at checkout"]
    end
    subgraph club["Shopee Members Club (status)"]
        direction TB
        Q["Completed orders AND spend<br/>in Jan-Jun or Jul-Dec"] --> TIER{{"Classic / Silver /<br/>Gold / Platinum"}}
        TIER --> V["Monthly vouchers<br/>and perks"]
    end
    E1 -. "also counts toward" .-> Q
```

| Aspect | Shopee's rule |
|---|---|
| Earning coins | Purchases, activities (check-in, games) and in-app tasks |
| Value | 100 coins = S$1 off a purchase; no minimum balance to redeem |
| Expiry | End of the third month after earning, counting the earning month as month 1 (January coins expire after 31 March) |
| Redemption caps | Vary by market and promotion (for example a share of the order value) |
| Members Club tiers | Classic, Silver, Gold, Platinum |
| Qualifying | **Both** a minimum number of completed orders **and** a minimum spend within the qualifying period. Singapore examples: Silver = 15 orders + S$250, Gold = 25 orders + S$1,000 |
| Qualifying period | Fixed half-years. Activity in 1 Jan-30 Jun qualifies you for 1 Jul-31 Dec, and vice versa. Reaching a tier mid-period applies immediately and lasts through the next period |
| Refunds | Returned, refunded or cancelled orders earn no qualifying points |
| Downgrade | If you don't requalify, you drop to the tier your activity supports when the period ends |

## 2. How it is configured in the engine

### 2.1 Concept mapping

| Shopee concept | Engine concept | Configuration |
|---|---|---|
| A user | Entity, keyed by `user_id` | `tenant.architecture_key` |
| Singapore calendar | Tenant time zone `Asia/Singapore` | Month-end expiry and half-year boundaries follow SGT |
| Shopee Coins | Primary point type `coins` | `expiry_mode: end_of_month`, `expiry_value: 3` |
| Redemption cap | Point type limit | `debit / day / 10,000` *(illustrative)*; percentage-of-order caps stay in checkout |
| Coins on orders | Rule effect `per_spend` | `rate_points: 1, rate_per: 100` = 1 coin per S$1 *(illustrative)*, spend in cents |
| Daily check-in | Rule effect `streak` | `steps: [5, 5, 10, 5, 5, 10, 25]` *(illustrative)* |
| Games and coins-cashback prizes | Rule effect `input` (credit) | Caller sends `amount` (max 500 and 5,000) |
| Redeem at checkout | Rule effect `input` (debit) | Checkout decides how many coins the order may use |
| A completed order counts toward tiers | `order_delta: 1` on `order_completed` | Order count and spend feed the levels |
| A refund doesn't count | `order_refunded`: `order_delta: -1` plus negative spend | Removes the order and its spend from the period |
| Classic / Silver / Gold / Platinum | Levels with ranks 1-4 and **`min_orders` + `min_spend`** | Silver 15 + S$250, Gold 25 + S$1,000, Platinum 50 + S$2,500 *(illustrative)* |
| Fixed half-year qualifying periods | Level policy `basis: calendar`, `period_months: 6` | Periods aligned to 1 January; a tier met in one period is kept through the next |
| Drop at the end of the period | `downgrade: true`, `grace_days: 0` | Applied by the level sweep at the boundary |
| Tier vouchers | Not points | Shopee's voucher service reads `level_slug` from the profile |

### 2.2 What gets created

```mermaid
flowchart TB
    T["Tenant: Shopee<br/>user_id · Asia/Singapore"]
    T --> PT["Point type: coins (primary)<br/>expire at the end of the 3rd month<br/>debit cap 10,000/day"]
    T --> TR["Tier: shopee (one rule package for everyone)"]
    TR --- R["order_completed · +1 coin per S$1 · orders +1<br/>order_refunded · orders -1 (negative spend)<br/>daily_check_in · streak 5,5,10,5,5,10,25<br/>shopee_games_reward · input, max 500, 20/day<br/>coins_cashback · input, max 5,000, 10/day<br/>redeem_at_checkout · input debit"]
    T --> LV["Levels (all linked to tier shopee)"]
    LV --- L["classic · rank 1 · no requirements<br/>silver · rank 2 · 15 orders + S$250<br/>gold · rank 3 · 25 orders + S$1,000<br/>platinum · rank 4 · 50 orders + S$2,500"]
    T --> POL["Level policy: calendar, 6-month periods<br/>downgrade on · grace 0"]
```

Members Club tiers don't change how coins are earned, so every level links the same `shopee`
tier. The levels are pure **status**, and the backend reads them to decide which vouchers a user
gets.

### 2.3 How coins expire

```mermaid
gantt
    dateFormat YYYY-MM-DD
    axisFormat %b
    title Coins earned in a month share one lot and expire together
    section Earned in January
    Valid (Jan, Feb, Mar)  :a1, 2026-01-01, 2026-04-01
    section Earned in February
    Valid (Feb, Mar, Apr)  :a2, 2026-02-01, 2026-05-01
    section Earned in March
    Valid (Mar, Apr, May)  :a3, 2026-03-01, 2026-06-01
```

All coins earned in one month (Singapore time) go into **one lot** that expires at midnight on the
1st of the fourth month. A redemption uses the oldest lot first, so January coins are spent
before February's. When a lot expires, the engine writes an `expiry` ledger entry for whatever
is left in it.

### 2.4 How Members Club tiers move

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Classic
    Classic --> Silver: 15 orders + S$250<br/>in the current half-year
    Silver --> Gold: 25 orders + S$1,000
    Gold --> Platinum: 50 orders + S$2,500
    Platinum --> Gold: period ends, and neither the current nor<br/>the previous half-year supports Platinum
    Gold --> Silver: same, at the next boundary
    Silver --> Classic: same
```

Under the `calendar` basis the engine checks **two windows**: the current half-year and the
previous one. A user keeps the better of the two tiers, which is exactly "Jan-Jun qualifies you
for Jul-Dec":

```mermaid
timeline
    title sg_alice reaches Silver in October 2026
    Jul-Dec 2026 : 15 completed orders and S$300 : Silver at once, since the current period qualifies
    Jan-Jun 2027 : no orders : still Silver, since the previous period (Jul-Dec 2026) qualifies
    From 1 Jul 2027 : the previous period is now Jan-Jun 2027 : drops to whatever Jan-Jun 2027 or Jul-Dec 2027 supports
```

Upgrades happen in the same transaction as the qualifying order. Period-boundary downgrades are
applied by the hourly level sweep.

## 3. How Shopee's backend talks to the engine

### 3.1 Integration architecture

```mermaid
flowchart LR
    subgraph shopee["Shopee platform"]
        OMS["Order service<br/>(on COMPLETED / REFUNDED)"]
        CO["Checkout service"]
        APP["App backend"]
        GAMES["Shopee Games"]
        VOUCH["Voucher service"]
    end
    subgraph engine["Points Engine"]
        EX["POST /engine/execute"]
        DATA["GET /data/profiles/..."]
    end
    OMS -->|"order_completed / order_refunded"| EX
    OMS -->|"coins_cashback"| EX
    CO -->|"redeem_at_checkout"| EX
    APP -->|"daily_check_in"| EX
    GAMES -->|"shopee_games_reward"| EX
    APP -->|"coins, tier progress, expiring coins"| DATA
    VOUCH -->|"level_slug decides voucher tier"| DATA
```

### 3.2 Order completion and refund

Shopee counts only **completed** orders toward tiers, so the order service calls the engine when
an order reaches *completed*, not when it is placed.

```mermaid
sequenceDiagram
    autonumber
    participant OMS as Order service
    participant E as Points Engine
    participant V as Voucher service
    OMS->>E: POST /engine/execute {user_id: sg_alice, rule: order_completed, spend: 2000, idempotency_key: shopee-order-50015}
    E-->>OMS: 201 {lines: [{coins, +20}], orders: 1, level_change: {to_level: silver, qualifying_orders: 15}}
    OMS-)V: level changed: issue Silver vouchers
    Note over OMS: Later, the order is returned
    OMS->>E: POST /engine/execute {rule: order_refunded, spend: -2000, idempotency_key: shopee-order-50015-refund}
    E-->>OMS: 201 {orders: -1, spend: -2000}
```

```sh
curl -X POST $ENGINE/engine/execute -H "Authorization: Bearer $SHOPEE_KEY" -d '{
  "user_id": "sg_alice", "rule": "order_completed", "spend": 2000,
  "idempotency_key": "shopee-order-50015"
}'
```

- Spend is in **cents** (S$20.00 = `2000`), and coins are `floor(spend / 100)`.
- If a seller voucher adds cashback coins, send a second call:
  `{"rule": "coins_cashback", "amount": 300, "idempotency_key": "shopee-order-60001-cashback"}`.
- A refund reverses spend and order count, so the order stops counting toward the half-year. Any
  coins earned are left alone. To claw them back, send
  `POST /admin/adjustments {"entity_id": "...", "points": -20, "reason": "refund of order 50015"}`.

### 3.3 Checkout redemption

```mermaid
sequenceDiagram
    autonumber
    participant CO as Checkout
    participant E as Points Engine
    CO->>E: GET /data/profiles/sg_alice
    E-->>CO: {balances: {coins: {balance: 305}}}
    CO->>CO: usable = min(balance, cap % x order value)
    CO->>E: POST /engine/execute {rule: redeem_at_checkout, amount: 200, idempotency_key: shopee-checkout-88001}
    E-->>CO: 201 {lines: [{coins, -200, balance_after: 105}]}
```

The coins that expire soonest are burned first. `422 insufficient_balance` (for example a
concurrent redemption on another device) and `422 point_limit_exceeded` (the daily cap) come back
before anything is deducted.

### 3.4 Showing coins and tier progress in the app

```sh
curl $ENGINE/data/profiles/sg_alice -H "Authorization: Bearer $SHOPEE_KEY"
curl $ENGINE/data/profiles/sg_alice/expirations -H "Authorization: Bearer $SHOPEE_KEY"
curl $ENGINE/admin/levels -H "Authorization: Bearer $SHOPEE_KEY"   # thresholds, to draw progress bars
```

```json
{
  "entity_id": "sg_alice", "balance": 105,
  "balances": { "coins": { "balance": 105, "lifetime_earned": 305, "qualifying_earned": 305 } },
  "lifetime_spend": 30000, "qualifying_spend": 30000,
  "lifetime_orders": 15, "qualifying_orders": 15,
  "level_slug": "silver", "below_level_since": null
}
```

```json
{ "data": [ { "point_type": "coins", "amount": 105, "expires_at": "2026-12-31T16:00:00Z" } ] }
```

`qualifying_orders` and `qualifying_spend` cover the **current half-year**, so the app can show
"10 more orders and S$700 more to reach Gold". `expires_at` is midnight on 1 January in
Singapore (16:00 UTC on 31 December), so the app can show "105 coins expire after 31 Dec".

### 3.5 Error handling

| Response | Meaning | Action |
|---|---|---|
| `200` + `replayed: true` | Duplicate delivery of the same order event | Treat as success |
| `422 insufficient_balance` | Not enough coins | Refresh the balance |
| `422 point_limit_exceeded` | Daily redemption cap reached | Show the remaining allowance |
| `422 amount_required` | `redeem_at_checkout` sent without `amount` | Bug in the caller |
| `422 spend_below_zero` / `orders_below_zero` | Refund larger than what was recorded | Check the original order was sent |
| `429 streak_already_claimed` | Already checked in today | `Retry-After` = seconds to midnight SGT |
| `503 limiter_unavailable` | Redis down | Retry with backoff, same idempotency key |

## 4. Seeded demo data

| User | What happened | Resulting state |
|---|---|---|
| `sg_alice` | 15 completed orders of S$20 (Silver on the 15th), check-in, redeemed 200 coins | 105 coins; 15 orders, S$300 this half-year; **Silver** |
| `sg_bob` | S$89.99 order (+89), 300 cashback coins, 120 game coins, order refunded, then tried to burn 50,000 coins (`insufficient_balance`) | 509 coins; 0 orders and S$0 this half-year; Classic |

## 5. Applying the configuration

```sh
go run ./cmd/seed -platform-token "$PLATFORM_ADMIN_TOKEN" \
  -keys-file seeds/tenant_keys.local.json -file seeds/shopee.json
```

To try another market, change the tenant time zone, thresholds and earn rate in the seed file:
Malaysia's Silver is 10 orders + RM500, Gold 30 + RM2,000 and Platinum 50 + RM4,000, with spend
in sen.
