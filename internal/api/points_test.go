package api_test

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

// tenantOf returns the tenant behind an API key, for store-level calls in tests.
func (h *harness) tenantOf(key string) store.Tenant {
	h.t.Helper()
	sum := sha256.Sum256([]byte(key))
	t, err := store.New(h.pool).GetTenantByAPIKeyHash(context.Background(), sum[:])
	if err != nil {
		h.t.Fatal(err)
	}
	return t
}

func (h *harness) newPointType(key string, body map[string]any) string {
	h.t.Helper()
	if _, ok := body["name"]; !ok {
		body["name"] = body["slug"]
	}
	return h.must(h.do("POST", "/admin/point-types", key, body), http.StatusCreated).body["id"].(string)
}

func (h *harness) run(key string, body map[string]any) resp {
	h.t.Helper()
	return h.do("POST", "/engine/execute", key, body)
}

func (h *harness) profile(key, entity string) resp {
	h.t.Helper()
	return h.must(h.do("GET", "/data/profiles/"+entity, key, nil), http.StatusOK)
}

// balanceOf reads one point type's balance from a profile response.
func balanceOf(p resp, pointType string) int64 {
	b, ok := p.body["balances"].(map[string]any)[pointType].(map[string]any)
	if !ok {
		return 0
	}
	return num(b["balance"])
}

func earnedOf(p resp, pointType, field string) int64 {
	b, ok := p.body["balances"].(map[string]any)[pointType].(map[string]any)
	if !ok {
		return 0
	}
	return num(b[field])
}

// lineDelta returns the delta an entry applied to pointType, or 0 if it has no such line.
func lineDelta(r resp, pointType string) int64 {
	for _, l := range r.body["lines"].([]any) {
		l := l.(map[string]any)
		if l["point_type"] == pointType {
			return num(l["delta"])
		}
	}
	return 0
}

func TestPointTypesCRUD(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")

	types := h.must(h.do("GET", "/admin/point-types", key, nil), http.StatusOK).body["data"].([]any)
	if len(types) != 1 || types[0].(map[string]any)["slug"] != "points" || types[0].(map[string]any)["is_primary"] != true {
		t.Fatalf("expected a default primary points type: %v", types)
	}
	pointsID := types[0].(map[string]any)["id"].(string)

	xpID := h.newPointType(key, map[string]any{"slug": "xp", "name": "Experience", "spendable": false})
	coinsID := h.newPointType(key, map[string]any{
		"slug": "coins", "expiry_mode": "end_of_month", "expiry_value": 3,
		"limits": []any{map[string]any{"direction": "debit", "period": "day", "max": 30000}},
	})
	h.must(h.do("POST", "/admin/point-types", key, map[string]any{"slug": "coins", "name": "dup"}), http.StatusConflict)
	for _, bad := range []map[string]any{
		{"slug": "gems", "name": "g", "expiry_mode": "days"},
		{"slug": "gems", "name": "g", "expiry_mode": "end_of_month", "expiry_value": 121},
		{"slug": "gems", "name": "g", "limits": []any{map[string]any{"direction": "debit", "period": "year", "max": 1}}},
		{"slug": "Gems", "name": "g"},
	} {
		h.must(h.do("POST", "/admin/point-types", key, bad), http.StatusBadRequest)
	}

	// Promoting coins demotes points; the primary can't be demoted or deleted directly.
	r := h.must(h.do("PATCH", "/admin/point-types/"+coinsID, key, map[string]any{"is_primary": true}), http.StatusOK)
	if r.body["is_primary"] != true || num(r.body["expiry_value"]) != 3 {
		t.Fatalf("unexpected coins: %v", r.body)
	}
	if p := h.must(h.do("GET", "/admin/point-types/"+pointsID, key, nil), http.StatusOK); p.body["is_primary"] != false {
		t.Fatalf("points should no longer be primary: %v", p.body)
	}
	if c := h.must(h.do("PATCH", "/admin/point-types/"+coinsID, key, map[string]any{"is_primary": false}), http.StatusConflict).errCode(); c != "primary_point_type" {
		t.Fatalf("unexpected error %q", c)
	}
	h.must(h.do("DELETE", "/admin/point-types/"+coinsID, key, nil), http.StatusConflict)

	// Switching expiry to never drops the value.
	r = h.must(h.do("PATCH", "/admin/point-types/"+coinsID, key, map[string]any{"expiry_mode": "never"}), http.StatusOK)
	if r.body["expiry_value"] != nil {
		t.Fatalf("expected expiry_value cleared: %v", r.body)
	}

	// The legacy point_delta shorthand now targets coins, the new primary.
	tierID := h.newTier(key, "basic")
	ruleID := h.newRule(key, tierID, map[string]any{"slug": "redeem", "point_delta": -5})
	rule := h.must(h.do("GET", "/admin/tiers/"+tierID+"/rules/"+ruleID, key, nil), http.StatusOK)
	e := rule.body["effects"].([]any)[0].(map[string]any)
	if e["point_type"] != "coins" || e["direction"] != "debit" || num(e["amount"]) != 5 {
		t.Fatalf("unexpected shorthand effect: %v", e)
	}
	// A type that a rule debits can't become non-spendable.
	if c := h.must(h.do("PATCH", "/admin/point-types/"+coinsID, key, map[string]any{"spendable": false}), http.StatusConflict).errCode(); c != "point_type_debited" {
		t.Fatalf("unexpected error %q", c)
	}
	// Rules can't debit non-spendable types, or name unknown ones.
	if c := h.must(h.do("POST", "/admin/tiers/"+tierID+"/rules", key, map[string]any{
		"slug": "burn_xp", "name": "x", "effects": []any{map[string]any{"point_type": "xp", "kind": "fixed", "direction": "debit", "amount": 1}},
	}), http.StatusUnprocessableEntity).errCode(); c != "point_type_not_spendable" {
		t.Fatalf("unexpected error %q", c)
	}
	if c := h.must(h.do("POST", "/admin/tiers/"+tierID+"/rules", key, map[string]any{
		"slug": "nope", "name": "x", "effects": []any{map[string]any{"point_type": "gems", "kind": "fixed", "amount": 1}},
	}), http.StatusNotFound).errCode(); c != "point_type_not_found" {
		t.Fatalf("unexpected error %q", c)
	}

	// Unused types can be deleted; types with history can't.
	h.must(h.do("DELETE", "/admin/point-types/"+pointsID, key, nil), http.StatusNoContent)
	h.newRule(key, tierID, map[string]any{"slug": "earn_xp", "effects": []any{map[string]any{"point_type": "xp", "kind": "fixed", "amount": 3}}})
	h.must(h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "earn_xp"}), http.StatusCreated)
	if c := h.must(h.do("DELETE", "/admin/point-types/"+xpID, key, nil), http.StatusConflict).errCode(); c != "point_type_in_use" {
		t.Fatalf("unexpected error %q", c)
	}
}

func TestMultiCurrencyRule(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	h.newPointType(key, map[string]any{"slug": "coins"})
	h.newPointType(key, map[string]any{"slug": "xp", "spendable": false})
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{
		"slug": "order_complete", "order_delta": 1,
		"effects": []any{
			map[string]any{"point_type": "coins", "kind": "per_spend", "rate_points": 1, "rate_per": 100, "max": 2000},
			map[string]any{"point_type": "xp", "kind": "fixed", "amount": 10},
		},
	})

	// 1,250.00 in paisa earns 1,250 coins and 10 XP and counts one order.
	r := h.must(h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "order_complete", "spend": 125000}), http.StatusCreated)
	if lineDelta(r, "coins") != 1250 || lineDelta(r, "xp") != 10 || num(r.body["orders"]) != 1 {
		t.Fatalf("unexpected entry: %v", r.body)
	}
	// The primary type (points) is untouched, so the legacy summary shows no change.
	if num(r.body["point_delta"]) != 0 || num(r.body["balance_after"]) != 0 {
		t.Fatalf("unexpected primary summary: %v", r.body)
	}
	// The per-order cap clamps a large order.
	r = h.must(h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "order_complete", "spend": 9_000_000}), http.StatusCreated)
	if lineDelta(r, "coins") != 2000 {
		t.Fatalf("expected the 2,000 cap: %v", r.body)
	}
	// A refund (negative spend) earns nothing.
	r = h.must(h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "order_complete", "spend": -100}), http.StatusCreated)
	if lineDelta(r, "coins") != 0 || lineDelta(r, "xp") != 10 {
		t.Fatalf("unexpected refund entry: %v", r.body)
	}

	p := h.profile(key, "u")
	if balanceOf(p, "coins") != 3250 || balanceOf(p, "xp") != 30 || num(p.body["balance"]) != 0 ||
		num(p.body["lifetime_orders"]) != 3 || num(p.body["lifetime_spend"]) != 9_124_900 {
		t.Fatalf("unexpected profile: %v", p.body)
	}
	if earnedOf(p, "coins", "lifetime_earned") != 3250 {
		t.Fatalf("unexpected lifetime_earned: %v", p.body)
	}

	// The ledger filters by point type, and the lines chain per type.
	coinLogs := h.must(h.do("GET", "/data/profiles/u/logs?point_type=coins&order=asc", key, nil), http.StatusOK).body["data"].([]any)
	if len(coinLogs) != 2 {
		t.Fatalf("expected 2 coin entries, got %d", len(coinLogs))
	}
	var prev int64
	for _, raw := range coinLogs {
		for _, l := range raw.(map[string]any)["lines"].([]any) {
			l := l.(map[string]any)
			if l["point_type"] != "coins" {
				continue
			}
			if num(l["balance_before"]) != prev {
				t.Fatalf("coin chain broken: %v", l)
			}
			prev = num(l["balance_after"])
		}
	}
	if all := h.must(h.do("GET", "/data/logs?point_type=xp", key, nil), http.StatusOK).body["data"].([]any); len(all) != 3 {
		t.Fatalf("expected 3 xp entries, got %d", len(all))
	}
}

func TestInputRedemption(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "point_delta": 500})
	h.newRule(key, tierID, map[string]any{
		"slug": "redeem", "effects": []any{map[string]any{"kind": "input", "direction": "debit", "min": 100, "max": 300}},
	})
	exec := func(rule string, amount any, idem string) resp {
		body := map[string]any{"user_id": "u", "tier": "basic", "rule": rule}
		if amount != nil {
			body["amount"] = amount
		}
		if idem != "" {
			body["idempotency_key"] = idem
		}
		return h.run(key, body)
	}

	h.must(exec("earn", nil, ""), http.StatusCreated)
	cases := []struct {
		rule   string
		amount any
		status int
		code   string
	}{
		{"redeem", nil, http.StatusUnprocessableEntity, "amount_required"},
		{"earn", 10, http.StatusUnprocessableEntity, "amount_not_accepted"},
		{"redeem", 50, http.StatusUnprocessableEntity, "amount_out_of_range"},
		{"redeem", 301, http.StatusUnprocessableEntity, "amount_out_of_range"},
		{"redeem", 0, http.StatusBadRequest, "validation_failed"},
		{"redeem", "lots", http.StatusBadRequest, "validation_failed"},
	}
	for _, tc := range cases {
		if c := h.must(exec(tc.rule, tc.amount, ""), tc.status).errCode(); c != tc.code {
			t.Fatalf("%s %v: expected %q, got %q", tc.rule, tc.amount, tc.code, c)
		}
	}

	r := h.must(exec("redeem", 270, "checkout-1"), http.StatusCreated)
	if num(r.body["point_delta"]) != -270 || num(r.body["balance_after"]) != 230 || num(r.body["amount"]) != 270 {
		t.Fatalf("unexpected redemption: %v", r.body)
	}
	// Replays must carry the same amount.
	h.must(exec("redeem", 270, "checkout-1"), http.StatusOK)
	if c := h.must(exec("redeem", 200, "checkout-1"), http.StatusUnprocessableEntity).errCode(); c != "idempotency_key_reused" {
		t.Fatalf("unexpected error %q", c)
	}
	if c := h.must(exec("redeem", 300, ""), http.StatusUnprocessableEntity).errCode(); c != "insufficient_balance" {
		t.Fatalf("unexpected error %q", c)
	}
}

func TestPeriodLimits(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	h.newPointType(key, map[string]any{"slug": "coins", "limits": []any{
		map[string]any{"direction": "credit", "period": "day", "max": 100},
		map[string]any{"direction": "debit", "period": "day", "max": 300},
		map[string]any{"direction": "debit", "period": "week", "max": 450},
	}})
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "effects": []any{map[string]any{"point_type": "coins", "kind": "fixed", "amount": 60}}})
	h.newRule(key, tierID, map[string]any{"slug": "redeem", "effects": []any{map[string]any{"point_type": "coins", "kind": "input", "direction": "debit"}}})
	earn := func() resp { return h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "earn"}) }
	redeem := func(n int) resp {
		return h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "redeem", "amount": n})
	}

	// Credits over the daily cap are clamped, down to nothing.
	if d := lineDelta(h.must(earn(), http.StatusCreated), "coins"); d != 60 {
		t.Fatalf("expected 60, got %d", d)
	}
	if d := lineDelta(h.must(earn(), http.StatusCreated), "coins"); d != 40 {
		t.Fatalf("expected the remaining 40, got %d", d)
	}
	r := h.must(earn(), http.StatusCreated)
	if len(r.body["lines"].([]any)) != 0 {
		t.Fatalf("expected no lines once the cap is used up: %v", r.body)
	}
	// Adjustments are not limited and don't count toward the caps.
	h.must(h.do("POST", "/admin/adjustments", key, map[string]any{"entity_id": "u", "point_type": "coins", "points": 1000, "reason": "goodwill"}), http.StatusCreated)

	// Debits over a cap are refused outright.
	h.must(redeem(250), http.StatusCreated)
	r = h.must(redeem(100), http.StatusUnprocessableEntity)
	if r.errCode() != "point_limit_exceeded" {
		t.Fatalf("unexpected error %v", r.body)
	}
	h.must(redeem(50), http.StatusCreated)
	if b := balanceOf(h.profile(key, "u"), "coins"); b != 1100-300 {
		t.Fatalf("unexpected balance %d", b)
	}
}

func TestExpiryLots(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	h.newPointType(key, map[string]any{"slug": "coins", "expiry_mode": "days", "expiry_value": 30})
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "effects": []any{map[string]any{"point_type": "coins", "kind": "fixed", "amount": 100}}})
	h.newRule(key, tierID, map[string]any{"slug": "bonus_forever", "effects": []any{
		map[string]any{"point_type": "coins", "kind": "fixed", "amount": 50, "expiry_mode": "never"},
	}})
	h.newRule(key, tierID, map[string]any{"slug": "redeem", "effects": []any{map[string]any{"point_type": "coins", "kind": "input", "direction": "debit"}}})
	entity := "u-" + uuid.NewString()
	exec := func(rule string, amount int) resp {
		body := map[string]any{"user_id": entity, "tier": "basic", "rule": rule}
		if amount > 0 {
			body["amount"] = amount
		}
		return h.run(key, body)
	}

	h.must(exec("earn", 0), http.StatusCreated)
	h.must(exec("bonus_forever", 0), http.StatusCreated)
	// Redemptions spend the expiring coins first.
	h.must(exec("redeem", 30), http.StatusCreated)
	exp := h.must(h.do("GET", "/data/profiles/"+entity+"/expirations", key, nil), http.StatusOK).body["data"].([]any)
	if len(exp) != 1 || num(exp[0].(map[string]any)["amount"]) != 70 || exp[0].(map[string]any)["point_type"] != "coins" {
		t.Fatalf("unexpected expirations: %v", exp)
	}
	at, _ := time.Parse(time.RFC3339Nano, exp[0].(map[string]any)["expires_at"].(string))
	if d := time.Until(at); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("expected expiry in ~30 days, got %v", d)
	}

	// Day 31: the sweep expires what's left of the lot.
	st := store.New(h.pool)
	asOf := time.Now().Add(31 * 24 * time.Hour)
	if _, _, err := st.ExpirePoints(context.Background(), &asOf); err != nil {
		t.Fatal(err)
	}
	p := h.profile(key, entity)
	if balanceOf(p, "coins") != 50 || earnedOf(p, "coins", "lifetime_earned") != 150 {
		t.Fatalf("unexpected profile after expiry: %v", p.body)
	}
	logs := h.must(h.do("GET", "/data/logs?entry_type=expiry&entity_id="+entity, key, nil), http.StatusOK).body["data"].([]any)
	if len(logs) != 1 {
		t.Fatalf("expected one expiry entry, got %v", logs)
	}
	e := logs[0].(map[string]any)
	if e["reason"] != "expired: 70 coins" || num(e["lines"].([]any)[0].(map[string]any)["delta"]) != -70 {
		t.Fatalf("unexpected expiry entry: %v", e)
	}
	if exp := h.must(h.do("GET", "/data/profiles/"+entity+"/expirations", key, nil), http.StatusOK).body["data"].([]any); len(exp) != 0 {
		t.Fatalf("expected no pending expirations: %v", exp)
	}

	// A lot that is already due can't be spent, even before the sweep runs.
	h.must(exec("earn", 0), http.StatusCreated)
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE point_lots SET expires_at = now() - interval '1 second' WHERE entity_id = $1`, entity); err != nil {
		t.Fatal(err)
	}
	if c := h.must(exec("redeem", 120), http.StatusUnprocessableEntity).errCode(); c != "insufficient_balance" {
		t.Fatalf("unexpected error %q", c)
	}
	r := h.must(exec("redeem", 50), http.StatusCreated)
	if num(r.body["lines"].([]any)[0].(map[string]any)["balance_before"]) != 50 {
		t.Fatalf("expected the due lot to have expired first: %v", r.body)
	}
	if logs := h.must(h.do("GET", "/data/logs?entry_type=expiry&entity_id="+entity, key, nil), http.StatusOK).body["data"].([]any); len(logs) != 2 {
		t.Fatalf("expected a second expiry entry, got %d", len(logs))
	}
}

func TestEndOfMonthLotsMerge(t *testing.T) {
	h := newHarness(t)
	r := h.must(h.do("POST", "/platform/tenants", platformToken,
		map[string]any{"name": "sg", "architecture_key": "user_id", "timezone": "Asia/Singapore"}), http.StatusCreated)
	if r.body["tenant"].(map[string]any)["timezone"] != "Asia/Singapore" {
		t.Fatalf("unexpected tenant: %v", r.body)
	}
	key := r.body["api_key"].(map[string]any)["secret"].(string)
	h.newPointType(key, map[string]any{"slug": "coins", "expiry_mode": "end_of_month", "expiry_value": 3})
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "effects": []any{map[string]any{"point_type": "coins", "kind": "fixed", "amount": 10}}})
	for i := 0; i < 3; i++ {
		h.must(h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "earn"}), http.StatusCreated)
	}
	exp := h.must(h.do("GET", "/data/profiles/u/expirations", key, nil), http.StatusOK).body["data"].([]any)
	if len(exp) != 1 || num(exp[0].(map[string]any)["amount"]) != 30 {
		t.Fatalf("expected one merged lot of 30: %v", exp)
	}
	sg, _ := time.LoadLocation("Asia/Singapore")
	now := time.Now().In(sg)
	want := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, sg).AddDate(0, 3, 0)
	got, _ := time.Parse(time.RFC3339Nano, exp[0].(map[string]any)["expires_at"].(string))
	if !got.Equal(want) {
		t.Fatalf("expected expiry at %v, got %v", want, got)
	}

	h.must(h.do("PATCH", "/admin/settings", key, map[string]any{"timezone": "Mars/Base"}), http.StatusBadRequest)
	s := h.must(h.do("PATCH", "/admin/settings", key, map[string]any{"timezone": "Asia/Dhaka"}), http.StatusOK)
	if s.body["timezone"] != "Asia/Dhaka" {
		t.Fatalf("unexpected settings: %v", s.body)
	}
	h.must(h.do("POST", "/platform/tenants", platformToken, map[string]any{"name": "x", "timezone": "Local"}), http.StatusBadRequest)
}

func TestStreaks(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "check_in", "effects": []any{
		map[string]any{"kind": "streak", "steps": []any{5, 10, 15}},
	}})
	entity := "u-" + uuid.NewString()
	claim := func() resp { return h.run(key, map[string]any{"user_id": entity, "tier": "basic", "rule": "check_in"}) }
	// shift moves the last claim back in time, as if days had passed.
	shift := func(days int) {
		if _, err := h.pool.Exec(context.Background(),
			`UPDATE entity_streaks SET last_day = last_day - $2::int WHERE entity_id = $1`, entity, days); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(r resp, delta int64, days int64) {
		t.Helper()
		if num(r.body["point_delta"]) != delta || num(r.body["streak"].(map[string]any)["days"]) != days {
			t.Fatalf("expected +%d on day %d: %v", delta, days, r.body)
		}
	}

	expect(h.must(claim(), http.StatusCreated), 5, 1)
	r := h.must(claim(), http.StatusTooManyRequests)
	if r.errCode() != "streak_already_claimed" || r.header.Get("Retry-After") == "" {
		t.Fatalf("expected a same-day rejection with Retry-After: %v", r.body)
	}
	shift(1)
	expect(h.must(claim(), http.StatusCreated), 10, 2)
	shift(1)
	expect(h.must(claim(), http.StatusCreated), 15, 3)
	// After the last step the rewards start over while the streak keeps counting.
	shift(1)
	expect(h.must(claim(), http.StatusCreated), 5, 4)
	// Missing a day restarts the streak.
	shift(2)
	expect(h.must(claim(), http.StatusCreated), 5, 1)

	// The streak carries over to another tier's rule with the same slug, e.g. after an upgrade.
	vipID := h.newTier(key, "vip")
	h.newRule(key, vipID, map[string]any{"slug": "check_in", "effects": []any{
		map[string]any{"kind": "streak", "steps": []any{50, 100, 150}},
	}})
	shift(1)
	expect(h.must(h.run(key, map[string]any{"user_id": entity, "tier": "vip", "rule": "check_in"}), http.StatusCreated), 100, 2)
	h.must(claim(), http.StatusTooManyRequests)
}

func TestMultiCriteriaLevels(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	h.newPointType(key, map[string]any{"slug": "gems"})
	h.newPointType(key, map[string]any{"slug": "xp", "spendable": false})
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "order", "order_delta": 1, "effects": []any{
		map[string]any{"point_type": "gems", "kind": "per_spend", "rate_points": 1, "rate_per": 1},
		map[string]any{"point_type": "xp", "kind": "fixed", "amount": 10},
	}})
	h.newRule(key, tierID, map[string]any{"slug": "cancel", "order_delta": -1})
	h.newRule(key, tierID, map[string]any{"slug": "redeem", "effects": []any{map[string]any{"point_type": "gems", "kind": "input", "direction": "debit"}}})
	h.newLevel(key, "silver", 1, 1000, map[string]any{"min_orders": 2})
	goldID := h.newLevel(key, "gold", 2, 5000, map[string]any{"min_orders": 3, "min_points": map[string]any{"xp": 30, "gems": 6000}})
	gold := h.must(h.do("GET", "/admin/levels/"+goldID, key, nil), http.StatusOK)
	if mp := gold.body["min_points"].(map[string]any); num(mp["xp"]) != 30 || num(mp["gems"]) != 6000 || num(gold.body["min_orders"]) != 3 {
		t.Fatalf("unexpected level: %v", gold.body)
	}
	order := func(spend int64) resp {
		return h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "order", "spend": spend})
	}

	// Spend alone isn't enough: Silver also needs two orders.
	if r := h.must(order(3000), http.StatusCreated); levelChange(r) != nil {
		t.Fatalf("one order shouldn't earn silver: %v", r.body)
	}
	r := h.must(order(600), http.StatusCreated)
	if c := levelChange(r); c["to_level"] != "silver" || num(c["qualifying_orders"]) != 2 {
		t.Fatalf("expected silver: %v", r.body)
	}
	// Gold needs 3 orders, 5,000 spend, 30 XP and 6,000 gems collected.
	r = h.must(order(2400), http.StatusCreated)
	c := levelChange(r)
	if c["to_level"] != "gold" || num(c["qualifying_spend"]) != 6000 ||
		num(c["qualifying_points"].(map[string]any)["xp"]) != 30 || num(c["qualifying_points"].(map[string]any)["gems"]) != 6000 {
		t.Fatalf("expected gold: %v", r.body)
	}
	// Redeeming gems doesn't reduce gems collected, so the level stays.
	h.must(h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "redeem", "amount": 5000}), http.StatusCreated)
	p := h.profile(key, "u")
	if levelOf(p) != "gold" || p.body["below_level_since"] != nil || earnedOf(p, "gems", "qualifying_earned") != 6000 ||
		num(p.body["qualifying_orders"]) != 3 {
		t.Fatalf("unexpected profile after redemption: %v", p.body)
	}
	// A cancellation drops the order count below Gold's requirement; with downgrades off the
	// level is kept but the entity is at risk.
	h.must(h.run(key, map[string]any{"user_id": "u", "tier": "basic", "rule": "cancel"}), http.StatusCreated)
	p = h.profile(key, "u")
	if levelOf(p) != "gold" || p.body["below_level_since"] == nil || num(p.body["lifetime_orders"]) != 2 {
		t.Fatalf("expected gold at risk: %v", p.body)
	}
	// Orders can't go below zero.
	h.must(h.do("POST", "/admin/adjustments", key, map[string]any{"entity_id": "u", "orders": -2, "reason": "fix"}), http.StatusCreated)
	if c := h.must(h.do("POST", "/admin/adjustments", key, map[string]any{"entity_id": "u", "orders": -1, "reason": "fix"}), http.StatusUnprocessableEntity).errCode(); c != "orders_below_zero" {
		t.Fatalf("unexpected error %q", c)
	}
	hist := h.must(h.do("GET", "/data/profiles/u/level-history?order=asc", key, nil), http.StatusOK).body["data"].([]any)
	if len(hist) != 2 || num(hist[1].(map[string]any)["qualifying_orders"]) != 3 {
		t.Fatalf("unexpected history: %v", hist)
	}
}

func TestCalendarBasis(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "order", "point_delta": 1, "order_delta": 1})
	h.newLevel(key, "gold", 1, 1000, map[string]any{"min_orders": 1})
	h.must(h.do("PATCH", "/admin/level-policy", key,
		map[string]any{"basis": "calendar", "period_months": 6, "downgrade": true}), http.StatusOK)

	entity := "u-" + uuid.NewString()
	r := h.must(h.run(key, map[string]any{"user_id": entity, "tier": "basic", "rule": "order", "spend": 1000}), http.StatusCreated)
	if levelChange(r)["to_level"] != "gold" {
		t.Fatalf("expected gold: %v", r.body)
	}

	tenant := h.tenantOf(key)
	st := store.New(h.pool)
	at := func(asOf time.Time) int {
		_, changed, err := st.ReevaluateLevels(context.Background(), tenant, &asOf, true)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	now := time.Now().UTC()
	first := time.Date(now.Year(), time.Month((int(now.Month())-1)/6*6+1), 1, 0, 0, 0, 0, time.UTC)
	next, afterNext := first.AddDate(0, 6, 0), first.AddDate(0, 12, 0)

	// Throughout the next period, the previous period's activity keeps Gold.
	if at(next.Add(24*time.Hour)) != 0 || at(afterNext.Add(-time.Hour)) != 0 {
		t.Fatal("gold should carry over through the next period")
	}
	p := h.profile(key, entity)
	if levelOf(p) != "gold" || num(p.body["qualifying_spend"]) != 0 {
		t.Fatalf("expected gold with no spend in the new period: %v", p.body)
	}
	// Once neither the current nor the previous period qualifies, it is lost.
	if at(afterNext.Add(time.Hour)) != 1 {
		t.Fatal("expected a downgrade two periods later")
	}
	if p := h.profile(key, entity); p.body["level_slug"] != nil {
		t.Fatalf("expected no level: %v", p.body)
	}

	pol := h.must(h.do("PATCH", "/admin/level-policy", key, map[string]any{"basis": "rolling", "window_days": 30}), http.StatusOK)
	if pol.body["period_months"] != nil || num(pol.body["window_days"]) != 30 {
		t.Fatalf("switching basis should drop period_months: %v", pol.body)
	}
}

func TestMinTenure(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "order", "point_delta": 1})
	h.newLevel(key, "club", 1, 1000, nil)
	h.must(h.do("PATCH", "/admin/level-policy", key, map[string]any{
		"basis": "rolling", "window_days": 30, "downgrade": true, "grace_days": 0, "min_tenure_days": 180,
	}), http.StatusOK)
	entity := "u-" + uuid.NewString()
	h.must(h.run(key, map[string]any{"user_id": entity, "tier": "basic", "rule": "order", "spend": 1000}), http.StatusCreated)

	tenant := h.tenantOf(key)
	st := store.New(h.pool)
	at := func(days int) int {
		asOf := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		_, changed, err := st.ReevaluateLevels(context.Background(), tenant, &asOf, true)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	// Spend ages out after 30 days, but the level was held for less than 180.
	if at(40) != 0 {
		t.Fatal("level lost before minimum tenure")
	}
	if p := h.profile(key, entity); levelOf(p) != "club" || p.body["below_level_since"] == nil {
		t.Fatalf("expected club at risk: %v", p.body)
	}
	if at(181) != 1 {
		t.Fatal("expected a downgrade after minimum tenure")
	}
}

func TestAdjustmentPointTypes(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	h.newPointType(key, map[string]any{"slug": "xp", "spendable": false})

	adj := map[string]any{"entity_id": "u", "point_type": "xp", "points": 40, "reason": "migration", "idempotency_key": "a1"}
	r := h.must(h.do("POST", "/admin/adjustments", key, adj), http.StatusCreated)
	if lineDelta(r, "xp") != 40 || num(r.body["point_delta"]) != 0 {
		t.Fatalf("unexpected adjustment: %v", r.body)
	}
	h.must(h.do("POST", "/admin/adjustments", key, adj), http.StatusOK)
	adj["point_type"] = "points"
	if c := h.must(h.do("POST", "/admin/adjustments", key, adj), http.StatusUnprocessableEntity).errCode(); c != "idempotency_key_reused" {
		t.Fatalf("unexpected error %q", c)
	}
	// Non-spendable types can be corrected by adjustments, but never below zero.
	h.must(h.do("POST", "/admin/adjustments", key, map[string]any{"entity_id": "u", "point_type": "xp", "points": -15, "reason": "fix"}), http.StatusCreated)
	r = h.must(h.do("POST", "/admin/adjustments", key, map[string]any{"entity_id": "u", "point_type": "xp", "points": -100, "reason": "fix"}), http.StatusUnprocessableEntity)
	if r.errCode() != "insufficient_balance" {
		t.Fatalf("unexpected error %v", r.body)
	}
	if c := h.must(h.do("POST", "/admin/adjustments", key, map[string]any{"entity_id": "u", "point_type": "gems", "points": 1, "reason": "x"}), http.StatusNotFound).errCode(); c != "point_type_not_found" {
		t.Fatalf("unexpected error %q", c)
	}
	p := h.profile(key, "u")
	if balanceOf(p, "xp") != 25 || earnedOf(p, "xp", "lifetime_earned") != 25 {
		t.Fatalf("unexpected profile: %v", p.body)
	}
}

func TestConcurrentRedemptionsKeepLotsConsistent(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	h.newPointType(key, map[string]any{"slug": "coins", "expiry_mode": "days", "expiry_value": 90})
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "effects": []any{map[string]any{"point_type": "coins", "kind": "fixed", "amount": 25}}})
	h.newRule(key, tierID, map[string]any{"slug": "redeem", "effects": []any{map[string]any{"point_type": "coins", "kind": "input", "direction": "debit"}}})
	entity := "u-" + uuid.NewString()
	for i := 0; i < 4; i++ {
		h.must(h.run(key, map[string]any{"user_id": entity, "tier": "basic", "rule": "earn"}), http.StatusCreated)
	}

	statuses := runConcurrently(20, func(int) int {
		return h.run(key, map[string]any{"user_id": entity, "tier": "basic", "rule": "redeem", "amount": 10}).status
	})
	if statuses[http.StatusCreated] != 10 || statuses[http.StatusUnprocessableEntity] != 10 {
		t.Fatalf("expected exactly 10 redemptions of 10 from 100: %v", statuses)
	}
	var balance, lots int64
	if err := h.pool.QueryRow(context.Background(), `
		SELECT b.balance, COALESCE((SELECT SUM(remaining) FROM point_lots l
			WHERE l.tenant_id = b.tenant_id AND l.entity_id = b.entity_id AND l.point_type_id = b.point_type_id), 0)
		FROM entity_balances b WHERE b.entity_id = $1`, entity).Scan(&balance, &lots); err != nil {
		t.Fatal(err)
	}
	if balance != 0 || lots != 0 {
		t.Fatalf("expected balance and lots to reach 0 together, got %d and %d", balance, lots)
	}
}
