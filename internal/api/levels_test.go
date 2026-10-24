package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

func (h *harness) spend(key, entity, tier, rule string, spend int64) resp {
	h.t.Helper()
	body := map[string]any{"entity_id": entity, "rule": rule, "spend": spend}
	if tier != "" {
		body["tier"] = tier
	}
	return h.do("POST", "/engine/execute", key, body)
}

func (h *harness) newLevel(key, slug string, rank int, minSpend int64, extra map[string]any) string {
	h.t.Helper()
	body := map[string]any{"slug": slug, "name": slug, "rank": rank, "min_spend": minSpend}
	for k, v := range extra {
		body[k] = v
	}
	return h.must(h.do("POST", "/admin/levels", key, body), http.StatusCreated).body["id"].(string)
}

func levelOf(r resp) any { return r.body["level_slug"] }

func levelChange(r resp) map[string]any {
	c, _ := r.body["level_change"].(map[string]any)
	return c
}

func TestLevelsLifetime(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "purchase", "point_delta": 100})
	h.newRule(key, tierID, map[string]any{"slug": "redeem", "point_delta": -100})
	h.newRule(key, tierID, map[string]any{"slug": "refund", "point_delta": -100})
	h.newLevel(key, "silver", 1, 5000, nil)
	h.newLevel(key, "gold", 2, 20000, nil)

	// Spend below the first threshold earns no level.
	r := h.must(h.spend(key, "u", "basic", "purchase", 1000), http.StatusCreated)
	if levelChange(r) != nil || num(r.body["spend"]) != 1000 {
		t.Fatalf("unexpected result: %v", r.body)
	}
	r = h.must(h.spend(key, "u", "basic", "purchase", 4000), http.StatusCreated)
	if c := levelChange(r); c["to_level"] != "silver" || c["direction"] != "up" || num(c["qualifying_spend"]) != 5000 {
		t.Fatalf("expected upgrade to silver, got %v", r.body)
	}

	// Redeeming points doesn't touch spend, so the level stays.
	h.must(h.spend(key, "u", "basic", "redeem", 0), http.StatusCreated)
	p := h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if levelOf(p) != "silver" || num(p.body["lifetime_spend"]) != 5000 || p.body["below_level_since"] != nil {
		t.Fatalf("unexpected profile after redemption: %v", p.body)
	}

	r = h.must(h.spend(key, "u", "basic", "purchase", 15000), http.StatusCreated)
	if c := levelChange(r); c["from_level"] != "silver" || c["to_level"] != "gold" {
		t.Fatalf("expected upgrade to gold, got %v", r.body)
	}

	// With downgrades off (the default), a refund starts the grace clock but keeps the level.
	h.must(h.spend(key, "u", "basic", "refund", -15000), http.StatusCreated)
	p = h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if levelOf(p) != "gold" || num(p.body["qualifying_spend"]) != 5000 || p.body["below_level_since"] == nil {
		t.Fatalf("unexpected profile after refund: %v", p.body)
	}
	// Refunding more than was ever spent is rejected.
	if c := h.must(h.spend(key, "u", "basic", "refund", -1_000_000), http.StatusUnprocessableEntity).errCode(); c != "spend_below_zero" {
		t.Fatalf("unexpected error %q", c)
	}

	// Turning downgrades on and re-evaluating applies the pending demotion.
	h.must(h.do("PATCH", "/admin/level-policy", key, map[string]any{"downgrade": true}), http.StatusOK)
	re := h.must(h.do("POST", "/admin/levels/reevaluate", key, nil), http.StatusOK)
	if num(re.body["evaluated"]) != 1 || num(re.body["changed"]) != 1 {
		t.Fatalf("unexpected re-evaluation: %v", re.body)
	}
	p = h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if levelOf(p) != "silver" || p.body["below_level_since"] != nil {
		t.Fatalf("expected silver after downgrade: %v", p.body)
	}
	if n := len(h.must(h.do("GET", "/data/profiles?level=silver", key, nil), http.StatusOK).body["data"].([]any)); n != 1 {
		t.Fatalf("expected one silver profile, got %d", n)
	}

	hist := h.must(h.do("GET", "/data/profiles/u/level-history?order=asc", key, nil), http.StatusOK).body["data"].([]any)
	var got []string
	for _, c := range hist {
		c := c.(map[string]any)
		got = append(got, c["direction"].(string)+":"+c["to_level"].(string))
	}
	if len(got) != 3 || got[0] != "up:silver" || got[1] != "up:gold" || got[2] != "down:silver" {
		t.Fatalf("unexpected level history: %v", got)
	}

	// Spend is part of the idempotency fingerprint.
	h.must(h.do("POST", "/engine/execute", key,
		map[string]any{"entity_id": "u", "tier": "basic", "rule": "purchase", "spend": 10, "idempotency_key": "p-1"}), http.StatusCreated)
	r = h.must(h.do("POST", "/engine/execute", key,
		map[string]any{"entity_id": "u", "tier": "basic", "rule": "purchase", "spend": 20, "idempotency_key": "p-1"}), http.StatusUnprocessableEntity)
	if r.errCode() != "idempotency_key_reused" {
		t.Fatalf("unexpected error %q", r.errCode())
	}

	// Adjustments can correct spend alone.
	adj := map[string]any{"entity_id": "u", "spend": 15000, "reason": "missed order"}
	r = h.must(h.do("POST", "/admin/adjustments", key, adj), http.StatusCreated)
	if c := levelChange(r); c["to_level"] != "gold" || num(r.body["point_delta"]) != 0 {
		t.Fatalf("expected spend adjustment to restore gold: %v", r.body)
	}
}

func TestLevelsRollingWithGrace(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "purchase", "point_delta": 10})
	h.newLevel(key, "gold", 1, 1000, nil)
	pol := h.must(h.do("PATCH", "/admin/level-policy", key,
		map[string]any{"basis": "rolling", "window_days": 30, "downgrade": true, "grace_days": 10}), http.StatusOK)
	if pol.body["basis"] != "rolling" || num(pol.body["window_days"]) != 30 {
		t.Fatalf("unexpected policy: %v", pol.body)
	}

	entity := "u-" + uuid.NewString()
	r := h.must(h.spend(key, entity, "basic", "purchase", 1000), http.StatusCreated)
	if levelChange(r)["to_level"] != "gold" {
		t.Fatalf("expected gold: %v", r.body)
	}

	// Evaluate at points in the future to simulate the spend ageing out of the window.
	ctx := context.Background()
	var tenantID uuid.UUID
	if err := h.pool.QueryRow(ctx,
		`SELECT tenant_id FROM entity_profiles WHERE entity_id = $1`, entity).Scan(&tenantID); err != nil {
		t.Fatal(err)
	}
	st := store.New(h.pool)
	window := 30
	policy := store.LevelPolicy{Basis: store.LevelBasisRolling, WindowDays: &window, Downgrade: true, GraceDays: 10}
	at := func(days int) int {
		asOf := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		tenant := store.Tenant{ID: tenantID, Timezone: "UTC", LevelPolicy: policy}
		_, changed, err := st.ReevaluateLevels(ctx, tenant, &asOf, true)
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}

	// Day 31: out of the window, but within grace.
	if at(31) != 0 {
		t.Fatal("level changed during grace period")
	}
	p := h.must(h.do("GET", "/data/profiles/"+entity, key, nil), http.StatusOK)
	if levelOf(p) != "gold" || num(p.body["qualifying_spend"]) != 0 || p.body["below_level_since"] == nil {
		t.Fatalf("unexpected profile in grace: %v", p.body)
	}
	// Day 42: grace has run out.
	if at(42) != 1 {
		t.Fatal("expected a downgrade after grace")
	}
	p = h.must(h.do("GET", "/data/profiles/"+entity, key, nil), http.StatusOK)
	if p.body["level_slug"] != nil || p.body["level_since"] != nil {
		t.Fatalf("expected no level: %v", p.body)
	}

	// Switching back to lifetime clears the window.
	pol = h.must(h.do("PATCH", "/admin/level-policy", key, map[string]any{"basis": "lifetime"}), http.StatusOK)
	if pol.body["window_days"] != nil {
		t.Fatalf("unexpected policy: %v", pol.body)
	}
}

func TestLevelTierResolution(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	basicID := h.newTier(key, "basic")
	goldTierID := h.newTier(key, "gold-perks")
	h.newRule(key, basicID, map[string]any{"slug": "purchase", "point_delta": 10})
	h.newRule(key, goldTierID, map[string]any{"slug": "purchase", "point_delta": 20})
	goldID := h.newLevel(key, "gold", 2, 1000, map[string]any{"tier_id": goldTierID})

	// No level yet, so a tier must be named.
	if c := h.must(h.spend(key, "u", "", "purchase", 0), http.StatusUnprocessableEntity).errCode(); c != "tier_unresolved" {
		t.Fatalf("unexpected error %q", c)
	}
	h.must(h.spend(key, "u", "basic", "purchase", 1000), http.StatusCreated)

	// A zero-threshold base level applies to brand-new entities, before they have a profile.
	h.newLevel(key, "base", 1, 0, map[string]any{"tier_id": basicID})
	r := h.must(h.spend(key, "newcomer", "", "purchase", 0), http.StatusCreated)
	if r.body["tier"] != "basic" || levelChange(r)["to_level"] != "base" {
		t.Fatalf("expected the base level's tier for a new entity: %v", r.body)
	}
	r = h.must(h.spend(key, "u", "", "purchase", 0), http.StatusCreated)
	if r.body["tier"] != "gold-perks" || num(r.body["point_delta"]) != 20 {
		t.Fatalf("expected the level's tier to apply: %v", r.body)
	}

	// Clearing the link with an explicit null.
	l := h.must(h.do("PATCH", "/admin/levels/"+goldID, key, map[string]any{"tier_id": nil}), http.StatusOK)
	if l.body["tier_id"] != nil || l.body["slug"] != "gold" {
		t.Fatalf("unexpected level: %v", l.body)
	}
	h.must(h.spend(key, "u", "", "purchase", 0), http.StatusUnprocessableEntity)
}

func TestLevelAdminValidation(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("")
	h.newLevel(key, "silver", 1, 5000, nil)

	cases := []struct {
		body   map[string]any
		status int
		code   string
	}{
		{map[string]any{"slug": "gold", "name": "Gold", "rank": 1, "min_spend": 9000}, http.StatusConflict, "level_rank_taken"},
		{map[string]any{"slug": "silver", "name": "Silver", "rank": 2, "min_spend": 1}, http.StatusConflict, "level_slug_taken"},
		{map[string]any{"slug": "gold", "name": "Gold", "min_spend": 9000}, http.StatusBadRequest, "validation_failed"},
		{map[string]any{"slug": "gold", "name": "Gold", "rank": 2, "min_spend": -1}, http.StatusBadRequest, "validation_failed"},
		{map[string]any{"slug": "gold", "name": "Gold", "rank": 2, "min_orders": -1}, http.StatusBadRequest, "validation_failed"},
		{map[string]any{"slug": "gold", "name": "Gold", "rank": 2, "min_points": map[string]any{"xp": 5}}, http.StatusNotFound, "point_type_not_found"},
		{map[string]any{"slug": "gold", "name": "Gold", "rank": 2, "min_spend": 9, "tier_id": uuid.NewString()}, http.StatusNotFound, "tier_not_found"},
	}
	for _, tc := range cases {
		if c := h.must(h.do("POST", "/admin/levels", key, tc.body), tc.status).errCode(); c != tc.code {
			t.Fatalf("%v: expected %q, got %q", tc.body, tc.code, c)
		}
	}

	for _, body := range []map[string]any{
		{"basis": "rolling"},
		{"basis": "weekly"},
		{"window_days": 30},
		{"grace_days": -1},
		{"basis": "calendar"},
		{"basis": "calendar", "period_months": 5},
		{"min_tenure_days": -3},
	} {
		h.must(h.do("PATCH", "/admin/level-policy", key, body), http.StatusBadRequest)
	}
	pol := h.must(h.do("GET", "/admin/level-policy", key, nil), http.StatusOK)
	if pol.body["basis"] != "lifetime" || pol.body["downgrade"] != false {
		t.Fatalf("unexpected default policy: %v", pol.body)
	}

	levels := h.must(h.do("GET", "/admin/levels", key, nil), http.StatusOK).body["data"].([]any)
	id := levels[0].(map[string]any)["id"].(string)
	h.must(h.do("PATCH", "/admin/levels/"+id+"/status", key, map[string]any{"is_active": false}), http.StatusOK)
	h.must(h.do("DELETE", "/admin/levels/"+id, key, nil), http.StatusNoContent)
	h.must(h.do("GET", "/admin/levels/"+id, key, nil), http.StatusNotFound)
}
