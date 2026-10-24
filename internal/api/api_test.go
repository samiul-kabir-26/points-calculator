package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/samiul-kabir-26/points-calculator/internal/api"
	"github.com/samiul-kabir-26/points-calculator/internal/database"
	"github.com/samiul-kabir-26/points-calculator/internal/engine"
	"github.com/samiul-kabir-26/points-calculator/internal/limiter"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

const platformToken = "test-platform-token"

type harness struct {
	t    *testing.T
	srv  *httptest.Server
	pool *pgxpool.Pool
}

// newHarness wires the real server to Postgres and Redis. Each test creates its
// own tenants, so tests don't interfere with each other or need cleanup.
func newHarness(t *testing.T) *harness {
	t.Helper()
	dbURL, redisURL := os.Getenv("TEST_DATABASE_URL"), os.Getenv("TEST_REDIS_URL")
	if dbURL == "" || redisURL == "" {
		t.Skip("TEST_DATABASE_URL and TEST_REDIS_URL must be set for integration tests")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, dbURL, 50)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := database.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	t.Cleanup(func() { rdb.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.New(pool)
	lim := limiter.New(rdb)
	srv := httptest.NewServer(api.NewServer(api.Deps{
		Pool: pool, Store: st, Engine: engine.New(st, lim, log), Limiter: lim,
		PlatformToken: platformToken, Logger: log,
	}).Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, pool: pool}
}

type resp struct {
	status int
	body   map[string]any
	header http.Header
}

func (r resp) errCode() string {
	e, _ := r.body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

func (h *harness) do(method, path, token string, body any) resp {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out := resp{status: res.StatusCode, header: res.Header}
	raw, _ := io.ReadAll(res.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			h.t.Fatalf("%s %s: non-JSON body %q", method, path, raw)
		}
	}
	return out
}

func (h *harness) must(r resp, status int) resp {
	h.t.Helper()
	if r.status != status {
		h.t.Fatalf("expected status %d, got %d: %v", status, r.status, r.body)
	}
	return r
}

// newTenant provisions a tenant and returns its API key. archKey may be empty.
func (h *harness) newTenant(archKey string) string {
	h.t.Helper()
	body := map[string]any{"name": "tenant-" + uuid.NewString()[:8]}
	if archKey != "" {
		body["architecture_key"] = archKey
	}
	r := h.must(h.do("POST", "/platform/tenants", platformToken, body), http.StatusCreated)
	return r.body["api_key"].(map[string]any)["secret"].(string)
}

func (h *harness) newTier(key, slug string) string {
	h.t.Helper()
	r := h.must(h.do("POST", "/admin/tiers", key, map[string]any{"slug": slug, "name": slug}), http.StatusCreated)
	return r.body["id"].(string)
}

func (h *harness) newRule(key, tierID string, rule map[string]any) string {
	h.t.Helper()
	if _, ok := rule["name"]; !ok {
		rule["name"] = rule["slug"]
	}
	r := h.must(h.do("POST", "/admin/tiers/"+tierID+"/rules", key, rule), http.StatusCreated)
	return r.body["id"].(string)
}

func (h *harness) execute(key, entity, tier, rule, idem string) resp {
	h.t.Helper()
	body := map[string]any{"entity_id": entity, "tier": tier, "rule": rule}
	if idem != "" {
		body["idempotency_key"] = idem
	}
	return h.do("POST", "/engine/execute", key, body)
}

func num(v any) int64 { return int64(v.(float64)) }

func TestAuthentication(t *testing.T) {
	h := newHarness(t)
	h.must(h.do("GET", "/admin/tiers", "", nil), http.StatusUnauthorized)
	h.must(h.do("GET", "/admin/tiers", "pce_bogus", nil), http.StatusUnauthorized)
	h.must(h.do("POST", "/platform/tenants", "wrong", map[string]any{"name": "x"}), http.StatusUnauthorized)

	key := h.newTenant("")
	h.must(h.do("GET", "/admin/tiers", key, nil), http.StatusOK)

	// Key rotation: create a second key, revoke the first, the first stops working.
	second := h.must(h.do("POST", "/admin/api-keys", key, map[string]any{"name": "rotated"}), http.StatusCreated)
	keys := h.must(h.do("GET", "/admin/api-keys", key, nil), http.StatusOK).body["data"].([]any)
	firstID := keys[0].(map[string]any)["id"].(string)
	h.must(h.do("DELETE", "/admin/api-keys/"+firstID, key, nil), http.StatusOK)
	h.must(h.do("GET", "/admin/tiers", key, nil), http.StatusUnauthorized)
	secondKey := second.body["secret"].(string)
	h.must(h.do("GET", "/admin/tiers", secondKey, nil), http.StatusOK)
	// The last active key can't be revoked.
	r := h.must(h.do("DELETE", "/admin/api-keys/"+second.body["id"].(string), secondKey, nil), http.StatusConflict)
	if r.errCode() != "last_active_api_key" {
		t.Fatalf("unexpected error %q", r.errCode())
	}
}

func TestEndToEndFlow(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("")

	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "daily_login", "point_delta": 10, "cooldown_seconds": 60})

	// Execution requires the architecture key to be configured first.
	r := h.must(h.execute(key, "user1", "basic", "daily_login", ""), http.StatusConflict)
	if r.errCode() != "architecture_not_configured" {
		t.Fatalf("unexpected error %q", r.errCode())
	}
	h.must(h.do("POST", "/admin/architecture", key, map[string]any{"key": "user_id"}), http.StatusOK)

	r = h.must(h.execute(key, "user1", "basic", "daily_login", ""), http.StatusCreated)
	if num(r.body["balance_after"]) != 10 || num(r.body["balance_before"]) != 0 {
		t.Fatalf("unexpected balances: %v", r.body)
	}

	// Cooldown blocks the immediate retrigger, with a Retry-After hint.
	r = h.must(h.execute(key, "user1", "basic", "daily_login", ""), http.StatusTooManyRequests)
	if r.errCode() != "cooldown_active" || r.header.Get("Retry-After") == "" {
		t.Fatalf("expected cooldown with Retry-After, got %v %v", r.body, r.header)
	}
	// Cooldowns are per entity.
	h.must(h.execute(key, "user2", "basic", "daily_login", ""), http.StatusCreated)

	// The entity may also be addressed by the architecture key name.
	h.newRule(key, tierID, map[string]any{"slug": "purchase", "point_delta": 25})
	r = h.must(h.do("POST", "/engine/execute", key,
		map[string]any{"user_id": "user1", "tier": "basic", "rule": "purchase"}), http.StatusCreated)
	if num(r.body["balance_after"]) != 35 {
		t.Fatalf("unexpected balance: %v", r.body)
	}

	p := h.must(h.do("GET", "/data/profiles/user1", key, nil), http.StatusOK)
	if num(p.body["balance"]) != 35 || p.body["tier_slug"] != "basic" || p.body["architecture_key"] != "user_id" {
		t.Fatalf("unexpected profile: %v", p.body)
	}
	h.must(h.do("GET", "/data/profiles/nobody", key, nil), http.StatusNotFound)

	logs := h.must(h.do("GET", "/data/profiles/user1/logs?order=asc", key, nil), http.StatusOK)
	data := logs.body["data"].([]any)
	if len(data) != 2 || data[0].(map[string]any)["rule"] != "daily_login" || data[1].(map[string]any)["rule"] != "purchase" {
		t.Fatalf("unexpected entity logs: %v", data)
	}

	all := h.must(h.do("GET", "/data/logs?rule=daily_login", key, nil), http.StatusOK).body["data"].([]any)
	if len(all) != 2 {
		t.Fatalf("expected 2 daily_login entries, got %d", len(all))
	}
	profiles := h.must(h.do("GET", "/data/profiles?tier=basic", key, nil), http.StatusOK).body["data"].([]any)
	if len(profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(profiles))
	}

	// Unknown tier / rule.
	if c := h.must(h.execute(key, "user1", "gold", "purchase", ""), http.StatusNotFound).errCode(); c != "tier_not_found" {
		t.Fatalf("unexpected error %q", c)
	}
	if c := h.must(h.execute(key, "user1", "basic", "nope", ""), http.StatusNotFound).errCode(); c != "rule_not_found" {
		t.Fatalf("unexpected error %q", c)
	}
}

func TestThrottle(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{
		"slug": "share", "point_delta": 1, "rate_limit_max": 3, "rate_limit_window_seconds": 3600,
	})
	for i := 0; i < 3; i++ {
		h.must(h.execute(key, "u", "basic", "share", ""), http.StatusCreated)
	}
	r := h.must(h.execute(key, "u", "basic", "share", ""), http.StatusTooManyRequests)
	if r.errCode() != "rate_limit_exceeded" || r.header.Get("Retry-After") == "" {
		t.Fatalf("expected rate_limit_exceeded with Retry-After, got %v", r.body)
	}
}

func TestThrottleUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{
		"slug": "share", "point_delta": 1, "rate_limit_max": 5, "rate_limit_window_seconds": 3600,
	})
	statuses := runConcurrently(30, func(int) int { return h.execute(key, "u", "basic", "share", "").status })
	if statuses[http.StatusCreated] != 5 || statuses[http.StatusTooManyRequests] != 25 {
		t.Fatalf("expected exactly 5 successes, got %v", statuses)
	}
}

func TestToggleTierAndRule(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "gold")
	ruleID := h.newRule(key, tierID, map[string]any{"slug": "bonus", "point_delta": 5})
	rulePath := "/admin/tiers/" + tierID + "/rules/" + ruleID

	h.must(h.do("PATCH", rulePath+"/status", key, map[string]any{"is_active": false}), http.StatusOK)
	if c := h.must(h.execute(key, "u", "gold", "bonus", ""), http.StatusForbidden).errCode(); c != "rule_inactive" {
		t.Fatalf("unexpected error %q", c)
	}
	h.must(h.do("PATCH", rulePath+"/status", key, map[string]any{"is_active": true}), http.StatusOK)
	h.must(h.execute(key, "u", "gold", "bonus", ""), http.StatusCreated)

	h.must(h.do("PATCH", "/admin/tiers/"+tierID+"/status", key, map[string]any{"is_active": false}), http.StatusOK)
	if c := h.must(h.execute(key, "u", "gold", "bonus", ""), http.StatusForbidden).errCode(); c != "tier_inactive" {
		t.Fatalf("unexpected error %q", c)
	}
}

func TestIdempotency(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "purchase", "point_delta": 100, "cooldown_seconds": 3600})
	h.newRule(key, tierID, map[string]any{"slug": "other", "point_delta": 1})

	first := h.must(h.execute(key, "u", "basic", "purchase", "order-1"), http.StatusCreated)
	// A retry returns the original result instead of hitting the cooldown or double-crediting.
	again := h.must(h.execute(key, "u", "basic", "purchase", "order-1"), http.StatusOK)
	if again.body["transaction_id"] != first.body["transaction_id"] || again.body["replayed"] != true ||
		again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("expected replay of original transaction, got %v", again.body)
	}
	p := h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if num(p.body["balance"]) != 100 {
		t.Fatalf("expected balance 100, got %v", p.body["balance"])
	}
	// Reusing the key for a different request is rejected.
	if c := h.must(h.execute(key, "u", "basic", "other", "order-1"), http.StatusUnprocessableEntity).errCode(); c != "idempotency_key_reused" {
		t.Fatalf("unexpected error %q", c)
	}
	// Keys are scoped per tenant.
	other := h.newTenant("user_id")
	otherTier := h.newTier(other, "basic")
	h.newRule(other, otherTier, map[string]any{"slug": "purchase", "point_delta": 100})
	h.must(h.execute(other, "u", "basic", "purchase", "order-1"), http.StatusCreated)
}

func TestIdempotencyUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "purchase", "point_delta": 7})

	statuses := runConcurrently(20, func(int) int { return h.execute(key, "u", "basic", "purchase", "same-key").status })
	if statuses[http.StatusCreated] != 1 {
		t.Fatalf("expected exactly one applied request, got %v", statuses)
	}
	for status := range statuses {
		if status != http.StatusCreated && status != http.StatusOK && status != http.StatusConflict {
			t.Fatalf("unexpected status mix %v", statuses)
		}
	}
	p := h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if num(p.body["balance"]) != 7 {
		t.Fatalf("expected balance 7, got %v", p.body["balance"])
	}
}

func TestConcurrentBalanceUpdates(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "tick", "point_delta": 3})

	const n = 100
	statuses := runConcurrently(n, func(int) int { return h.execute(key, "u", "basic", "tick", "").status })
	if statuses[http.StatusCreated] != n {
		t.Fatalf("expected %d successes, got %v", n, statuses)
	}
	p := h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if num(p.body["balance"]) != 3*n {
		t.Fatalf("expected balance %d, got %v", 3*n, p.body["balance"])
	}
	// The ledger must form an unbroken chain: each entry starts where the previous ended.
	logs := h.must(h.do("GET", "/data/profiles/u/logs?order=asc&limit=500", key, nil), http.StatusOK).body["data"].([]any)
	var prev int64
	for i, raw := range logs {
		e := raw.(map[string]any)
		if num(e["balance_before"]) != prev || num(e["balance_after"]) != prev+3 {
			t.Fatalf("ledger chain broken at entry %d: %v", i, e)
		}
		prev = num(e["balance_after"])
	}
}

func TestInsufficientBalanceReleasesLimit(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "point_delta": 50})
	h.newRule(key, tierID, map[string]any{"slug": "redeem", "point_delta": -100, "cooldown_seconds": 3600})

	if c := h.must(h.execute(key, "u", "basic", "redeem", ""), http.StatusUnprocessableEntity).errCode(); c != "insufficient_balance" {
		t.Fatalf("unexpected error %q", c)
	}
	h.must(h.execute(key, "u", "basic", "earn", ""), http.StatusCreated)
	h.must(h.execute(key, "u", "basic", "earn", ""), http.StatusCreated)
	// The failed redemptions must not have started the cooldown.
	r := h.must(h.execute(key, "u", "basic", "redeem", ""), http.StatusCreated)
	if num(r.body["balance_after"]) != 0 {
		t.Fatalf("unexpected balance: %v", r.body)
	}
}

func TestAdjustments(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "point_delta": 40})
	h.must(h.execute(key, "u", "basic", "earn", ""), http.StatusCreated)

	adj := map[string]any{"entity_id": "u", "points": -15, "reason": "fraud reversal", "idempotency_key": "adj-1"}
	r := h.must(h.do("POST", "/admin/adjustments", key, adj), http.StatusCreated)
	if num(r.body["balance_after"]) != 25 || r.body["entry_type"] != "adjustment" || r.body["reason"] != "fraud reversal" {
		t.Fatalf("unexpected adjustment: %v", r.body)
	}
	h.must(h.do("POST", "/admin/adjustments", key, adj), http.StatusOK)
	over := map[string]any{"entity_id": "u", "points": -1000, "reason": "too much"}
	h.must(h.do("POST", "/admin/adjustments", key, over), http.StatusUnprocessableEntity)

	adjLogs := h.must(h.do("GET", "/data/logs?entry_type=adjustment", key, nil), http.StatusOK).body["data"].([]any)
	if len(adjLogs) != 1 {
		t.Fatalf("expected one adjustment entry, got %d", len(adjLogs))
	}
	// Profile tier is untouched by adjustments.
	p := h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if p.body["tier_slug"] != "basic" || num(p.body["balance"]) != 25 {
		t.Fatalf("unexpected profile: %v", p.body)
	}
}

func TestLedgerIsAppendOnly(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "point_delta": 1})
	r := h.must(h.execute(key, "u", "basic", "earn", ""), http.StatusCreated)
	txID := r.body["transaction_id"]

	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `UPDATE ledger_entries SET point_delta = 999 WHERE transaction_id = $1`, txID); err == nil {
		t.Fatal("expected UPDATE on ledger_entries to fail")
	}
	if _, err := h.pool.Exec(ctx, `DELETE FROM ledger_entries WHERE transaction_id = $1`, txID); err == nil {
		t.Fatal("expected DELETE on ledger_entries to fail")
	}

	// Deleting the tier (and its rules) keeps the history intact.
	h.must(h.do("DELETE", "/admin/tiers/"+tierID, key, nil), http.StatusNoContent)
	logs := h.must(h.do("GET", "/data/profiles/u/logs", key, nil), http.StatusOK).body["data"].([]any)
	if len(logs) != 1 || logs[0].(map[string]any)["tier"] != "basic" {
		t.Fatalf("expected history to survive tier deletion, got %v", logs)
	}
	p := h.must(h.do("GET", "/data/profiles/u", key, nil), http.StatusOK)
	if p.body["tier_id"] != nil {
		t.Fatalf("expected profile tier to be cleared, got %v", p.body)
	}
}

func TestTenantIsolation(t *testing.T) {
	h := newHarness(t)
	a := h.newTenant("user_id")
	b := h.newTenant("user_id")
	tierA := h.newTier(a, "basic")
	ruleA := h.newRule(a, tierA, map[string]any{"slug": "earn", "point_delta": 5})
	h.must(h.execute(a, "shared-id", "basic", "earn", ""), http.StatusCreated)

	h.must(h.do("GET", "/admin/tiers/"+tierA, b, nil), http.StatusNotFound)
	h.must(h.do("GET", "/admin/tiers/"+tierA+"/rules/"+ruleA, b, nil), http.StatusNotFound)
	h.must(h.do("PATCH", "/admin/tiers/"+tierA+"/status", b, map[string]any{"is_active": false}), http.StatusNotFound)
	h.must(h.do("DELETE", "/admin/tiers/"+tierA, b, nil), http.StatusNotFound)
	h.must(h.do("POST", "/admin/tiers/"+tierA+"/rules", b, map[string]any{"slug": "x", "name": "x", "point_delta": 1}), http.StatusNotFound)
	h.must(h.execute(b, "shared-id", "basic", "earn", ""), http.StatusNotFound)
	h.must(h.do("GET", "/data/profiles/shared-id", b, nil), http.StatusNotFound)
	if logs := h.must(h.do("GET", "/data/logs", b, nil), http.StatusOK).body["data"].([]any); len(logs) != 0 {
		t.Fatalf("tenant B sees tenant A logs: %v", logs)
	}
	if tiers := h.must(h.do("GET", "/admin/tiers", b, nil), http.StatusOK).body["data"].([]any); len(tiers) != 0 {
		t.Fatalf("tenant B sees tenant A tiers: %v", tiers)
	}
	// The same slugs are independent per tenant.
	h.newTier(b, "basic")
}

func TestArchitectureLock(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("")
	h.must(h.do("POST", "/admin/architecture", key, map[string]any{"key": "user_id"}), http.StatusOK)
	h.must(h.do("POST", "/admin/architecture", key, map[string]any{"key": "wallet_address"}), http.StatusOK)
	h.must(h.do("POST", "/admin/architecture", key, map[string]any{"key": "not valid!"}), http.StatusBadRequest)

	tierID := h.newTier(key, "basic")
	h.newRule(key, tierID, map[string]any{"slug": "earn", "point_delta": 1})
	h.must(h.execute(key, "0xabc", "basic", "earn", ""), http.StatusCreated)

	r := h.must(h.do("POST", "/admin/architecture", key, map[string]any{"key": "user_id"}), http.StatusConflict)
	if r.errCode() != "architecture_locked" {
		t.Fatalf("unexpected error %q", r.errCode())
	}
	// Re-setting the same key is a harmless no-op.
	h.must(h.do("POST", "/admin/architecture", key, map[string]any{"key": "wallet_address"}), http.StatusOK)
}

func TestAdminValidationAndCRUD(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	h.must(h.do("POST", "/admin/tiers", key, map[string]any{"slug": "Bad Slug", "name": "x"}), http.StatusBadRequest)
	h.must(h.do("POST", "/admin/tiers", key, map[string]any{"slug": "ok", "name": "x", "bogus": 1}), http.StatusBadRequest)
	tierID := h.newTier(key, "basic")
	h.must(h.do("POST", "/admin/tiers", key, map[string]any{"slug": "basic", "name": "dup"}), http.StatusConflict)
	h.must(h.do("GET", "/admin/tiers/not-a-uuid", key, nil), http.StatusBadRequest)

	rules := "/admin/tiers/" + tierID + "/rules"
	h.must(h.do("POST", rules, key, map[string]any{"slug": "z", "name": "z", "point_delta": 0}), http.StatusBadRequest)
	h.must(h.do("POST", rules, key, map[string]any{"slug": "z", "name": "z", "point_delta": 1, "rate_limit_max": 5}), http.StatusBadRequest)
	ruleID := h.newRule(key, tierID, map[string]any{"slug": "earn", "point_delta": 1})
	h.must(h.do("POST", rules, key, map[string]any{"slug": "earn", "name": "dup", "point_delta": 1}), http.StatusConflict)

	r := h.must(h.do("PATCH", rules+"/"+ruleID, key, map[string]any{"point_delta": 20, "cooldown_seconds": 5}), http.StatusOK)
	effects := r.body["effects"].([]any)
	if len(effects) != 1 || num(effects[0].(map[string]any)["amount"]) != 20 ||
		effects[0].(map[string]any)["point_type"] != "points" || num(r.body["cooldown_seconds"]) != 5 || r.body["slug"] != "earn" {
		t.Fatalf("unexpected rule after patch: %v", r.body)
	}
	// point_delta and effects are alternatives.
	h.must(h.do("PATCH", rules+"/"+ruleID, key, map[string]any{"point_delta": 5, "effects": []any{}}), http.StatusBadRequest)
	// An update producing an invalid combination is rejected and leaves the rule unchanged.
	h.must(h.do("PATCH", rules+"/"+ruleID, key, map[string]any{"rate_limit_window_seconds": 60}), http.StatusBadRequest)
	got := h.must(h.do("GET", rules+"/"+ruleID, key, nil), http.StatusOK)
	if num(got.body["rate_limit_window_seconds"]) != 0 {
		t.Fatalf("invalid patch was persisted: %v", got.body)
	}
	if list := h.must(h.do("GET", rules, key, nil), http.StatusOK).body["data"].([]any); len(list) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(list))
	}
	h.must(h.do("PATCH", "/admin/tiers/"+tierID, key, map[string]any{"name": "Basic Plan"}), http.StatusOK)
	h.must(h.do("DELETE", rules+"/"+ruleID, key, nil), http.StatusNoContent)
	h.must(h.do("GET", rules+"/"+ruleID, key, nil), http.StatusNotFound)
	h.must(h.do("DELETE", "/admin/tiers/"+tierID, key, nil), http.StatusNoContent)
	h.must(h.do("GET", "/admin/tiers/"+tierID, key, nil), http.StatusNotFound)
}

func TestLogPaginationAndFilters(t *testing.T) {
	h := newHarness(t)
	key := h.newTenant("user_id")
	basic := h.newTier(key, "basic")
	gold := h.newTier(key, "gold")
	h.newRule(key, basic, map[string]any{"slug": "earn", "point_delta": 1})
	h.newRule(key, gold, map[string]any{"slug": "earn", "point_delta": 2})
	for i := 0; i < 5; i++ {
		h.must(h.execute(key, fmt.Sprintf("u%d", i), "basic", "earn", ""), http.StatusCreated)
	}
	h.must(h.execute(key, "u0", "gold", "earn", ""), http.StatusCreated)

	seen := map[float64]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		path := "/data/logs?tier=basic&limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		r := h.must(h.do("GET", path, key, nil), http.StatusOK)
		for _, e := range r.body["data"].([]any) {
			seen[e.(map[string]any)["sequence"].(float64)] = true
		}
		next, _ := r.body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("expected 5 distinct basic entries across pages, got %d", len(seen))
	}

	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if logs := h.must(h.do("GET", "/data/logs?from="+future, key, nil), http.StatusOK).body["data"].([]any); len(logs) != 0 {
		t.Fatalf("expected no entries after %s, got %d", future, len(logs))
	}
	h.must(h.do("GET", "/data/logs?from=yesterday", key, nil), http.StatusBadRequest)

	// Profiles paginate too.
	r := h.must(h.do("GET", "/data/profiles?limit=3", key, nil), http.StatusOK)
	next := r.body["next_cursor"].(string)
	r2 := h.must(h.do("GET", "/data/profiles?limit=3&cursor="+next, key, nil), http.StatusOK)
	if len(r.body["data"].([]any))+len(r2.body["data"].([]any)) != 5 || r2.body["next_cursor"] != nil {
		t.Fatalf("unexpected profile pagination: %v / %v", r.body, r2.body)
	}
	// u0 last executed under gold, so it is the only gold profile.
	if gp := h.must(h.do("GET", "/data/profiles?tier=gold", key, nil), http.StatusOK).body["data"].([]any); len(gp) != 1 {
		t.Fatalf("expected 1 gold profile, got %d", len(gp))
	}
}

// runConcurrently calls fn n times in parallel and tallies the returned status codes.
func runConcurrently(n int, fn func(i int) int) map[int]int {
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		statuses = map[int]int{}
		start    = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s := fn(i)
			mu.Lock()
			statuses[s]++
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	return statuses
}
