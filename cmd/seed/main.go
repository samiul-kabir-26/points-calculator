// Command seed applies a loyalty program file (settings, point types, tiers, rules, levels and
// the level policy) to a tenant through the admin API, then optionally runs a list of demo
// transactions. It is idempotent: point types, tiers, rules and levels are matched by slug,
// created when missing and updated when they differ, and demo steps carry idempotency keys.
// Anything not in the file is left alone. The level policy, if given, replaces the tenant's,
// and every profile is then re-evaluated.
//
// Seed an existing tenant:
//
//	go run ./cmd/seed -key pce_... -file seeds/loyalty_program.json
//
// Or let the tool create the tenant named in the file's "tenant" section and remember its key
// in a keys file, so later runs reuse it:
//
//	go run ./cmd/seed -platform-token change-me -keys-file seeds/tenant_keys.local.json -file seeds/daraz.json
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type tenantSpec struct {
	Name            string `json:"name"`
	ArchitectureKey string `json:"architecture_key"`
	Timezone        string `json:"timezone,omitempty"`
}

type pointType struct {
	ID          string           `json:"id,omitempty"`
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	IsPrimary   bool             `json:"is_primary"`
	Spendable   *bool            `json:"spendable,omitempty"`
	ExpiryMode  string           `json:"expiry_mode,omitempty"`
	ExpiryValue *int             `json:"expiry_value"`
	Limits      []map[string]any `json:"limits"`
}

type tier struct {
	ID          string `json:"id,omitempty"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
	Rules       []rule `json:"rules,omitempty"`
}

type rule struct {
	ID          string `json:"id,omitempty"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// PointDelta is shorthand for one fixed effect on the primary point type.
	PointDelta             *int64           `json:"point_delta,omitempty"`
	Effects                []map[string]any `json:"effects,omitempty"`
	OrderDelta             int              `json:"order_delta"`
	CooldownSeconds        int              `json:"cooldown_seconds"`
	RateLimitMax           int              `json:"rate_limit_max"`
	RateLimitWindowSeconds int              `json:"rate_limit_window_seconds"`
	IsActive               *bool            `json:"is_active,omitempty"`
}

type level struct {
	ID          string           `json:"id,omitempty"`
	Slug        string           `json:"slug"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Rank        int              `json:"rank"`
	MinSpend    int64            `json:"min_spend"`
	MinOrders   int64            `json:"min_orders"`
	MinPoints   map[string]int64 `json:"min_points"`
	// Tier is the slug of the linked tier in the seed file; the API takes its ID.
	Tier     string  `json:"tier,omitempty"`
	TierID   *string `json:"tier_id"`
	IsActive *bool   `json:"is_active,omitempty"`
}

type levelPolicy struct {
	Basis         string `json:"basis"`
	WindowDays    *int   `json:"window_days,omitempty"`
	PeriodMonths  *int   `json:"period_months,omitempty"`
	Downgrade     bool   `json:"downgrade"`
	GraceDays     int    `json:"grace_days"`
	MinTenureDays int    `json:"min_tenure_days"`
}

// demoStep is one transaction to run after the program is applied. Exactly one of Execute and
// Adjust is set; Expect names an error code the step is meant to produce.
type demoStep struct {
	Note    string         `json:"note"`
	Execute map[string]any `json:"execute,omitempty"`
	Adjust  map[string]any `json:"adjust,omitempty"`
	Expect  string         `json:"expect,omitempty"`
}

type seedFile struct {
	Tenant      *tenantSpec  `json:"tenant"`
	PointTypes  []pointType  `json:"point_types"`
	Tiers       []tier       `json:"tiers"`
	LevelPolicy *levelPolicy `json:"level_policy"`
	Levels      []level      `json:"levels"`
	Demo        []demoStep   `json:"demo"`
}

type apiError struct {
	status int
	Code   string `json:"code"`
	Msg    string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s: %s", e.status, e.Code, e.Msg) }

type client struct {
	base, key string
	http      *http.Client
}

func (c *client) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		var e struct{ Error apiError }
		if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			e.Error.status = res.StatusCode
			return fmt.Errorf("%s %s: %w", method, path, &e.Error)
		}
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func main() {
	base := flag.String("url", "http://localhost:8080", "API base URL")
	key := flag.String("key", os.Getenv("PC_API_KEY"), "tenant API key (or PC_API_KEY)")
	file := flag.String("file", "seeds/loyalty_program.json", "seed file")
	platform := flag.String("platform-token", os.Getenv("PLATFORM_ADMIN_TOKEN"),
		"platform token, used to create the file's tenant when no key is known (or PLATFORM_ADMIN_TOKEN)")
	keysFile := flag.String("keys-file", "", "JSON file that remembers created tenants' API keys by tenant name")
	noDemo := flag.Bool("no-demo", false, "skip the file's demo transactions")
	flag.Parse()

	seed, err := readSeed(*file)
	if err != nil {
		fail(err)
	}
	c := &client{base: strings.TrimRight(*base, "/"), key: *key, http: &http.Client{Timeout: 30 * time.Second}}
	if c.key == "" {
		if c.key, err = tenantKey(c, seed.Tenant, *platform, *keysFile); err != nil {
			fail(err)
		}
	}
	if err := apply(c, seed); err != nil {
		fail(err)
	}
	if !*noDemo {
		if err := runDemo(c, seed.Demo); err != nil {
			fail(err)
		}
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "seed failed:", err)
	os.Exit(1)
}

func readSeed(file string) (seedFile, error) {
	var seed seedFile
	raw, err := os.ReadFile(file)
	if err != nil {
		return seed, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&seed); err != nil {
		return seed, fmt.Errorf("parse %s: %w", file, err)
	}
	return seed, nil
}

type keyEntry struct {
	TenantID string `json:"tenant_id"`
	APIKey   string `json:"api_key"`
}

// tenantKey returns the API key remembered for the file's tenant, creating the tenant with the
// platform token (and remembering its key) when there is none yet.
func tenantKey(c *client, spec *tenantSpec, platformToken, keysFile string) (string, error) {
	if spec == nil {
		return "", errors.New("missing -key (or PC_API_KEY), and the seed file has no tenant section")
	}
	keys := map[string]keyEntry{}
	if keysFile != "" {
		raw, err := os.ReadFile(keysFile)
		switch {
		case err == nil:
			if err := json.Unmarshal(raw, &keys); err != nil {
				return "", fmt.Errorf("parse %s: %w", keysFile, err)
			}
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
		if k, ok := keys[spec.Name]; ok {
			fmt.Printf("tenant %q: using the key in %s\n", spec.Name, keysFile)
			return k.APIKey, nil
		}
	}
	if platformToken == "" {
		return "", errors.New("missing -key, and no -platform-token to create the tenant with")
	}
	var created struct {
		Tenant struct{ ID string }     `json:"tenant"`
		APIKey struct{ Secret string } `json:"api_key"`
	}
	pc := &client{base: c.base, key: platformToken, http: c.http}
	if err := pc.do("POST", "/platform/tenants", spec, &created); err != nil {
		return "", err
	}
	fmt.Printf("tenant %q created: %s\n", spec.Name, created.Tenant.ID)
	if keysFile == "" {
		fmt.Printf("API key (shown once): %s\n", created.APIKey.Secret)
		return created.APIKey.Secret, nil
	}
	keys[spec.Name] = keyEntry{TenantID: created.Tenant.ID, APIKey: created.APIKey.Secret}
	raw, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(keysFile, append(raw, '\n'), 0o600); err != nil {
		return "", err
	}
	fmt.Printf("API key saved to %s\n", keysFile)
	return created.APIKey.Secret, nil
}

func apply(c *client, seed seedFile) error {
	if seed.Tenant != nil && seed.Tenant.Timezone != "" {
		if err := c.do("PATCH", "/admin/settings", map[string]string{"timezone": seed.Tenant.Timezone}, nil); err != nil {
			return err
		}
		fmt.Printf("timezone       %s\n", seed.Tenant.Timezone)
	}
	if seed.Tenant != nil && seed.Tenant.ArchitectureKey != "" {
		if err := c.do("POST", "/admin/architecture", map[string]string{"key": seed.Tenant.ArchitectureKey}, nil); err != nil {
			return err
		}
	}
	if err := syncPointTypes(c, seed.PointTypes); err != nil {
		return err
	}

	var existing struct{ Data []tier }
	if err := c.do("GET", "/admin/tiers", nil, &existing); err != nil {
		return err
	}
	tiersBySlug := map[string]tier{}
	for _, t := range existing.Data {
		tiersBySlug[t.Slug] = t
	}

	for _, want := range seed.Tiers {
		rules := want.Rules
		want.Rules = nil
		var got tier
		if cur, ok := tiersBySlug[want.Slug]; ok {
			if err := c.do("PATCH", "/admin/tiers/"+cur.ID, want, &got); err != nil {
				return err
			}
			fmt.Printf("tier %-14s updated\n", want.Slug)
		} else {
			if err := c.do("POST", "/admin/tiers", want, &got); err != nil {
				return err
			}
			fmt.Printf("tier %-14s created\n", want.Slug)
		}
		tiersBySlug[got.Slug] = got
		if err := syncRules(c, got.ID, rules); err != nil {
			return fmt.Errorf("tier %s: %w", want.Slug, err)
		}
	}

	if seed.LevelPolicy != nil {
		if err := c.do("PATCH", "/admin/level-policy", seed.LevelPolicy, nil); err != nil {
			return err
		}
		fmt.Printf("level policy   %s\n", seed.LevelPolicy.Basis)
	}
	if len(seed.Levels) == 0 {
		return nil
	}
	if err := syncLevels(c, seed.Levels, tiersBySlug); err != nil {
		return err
	}
	var re struct{ Evaluated, Changed int }
	if err := c.do("POST", "/admin/levels/reevaluate", nil, &re); err != nil {
		return err
	}
	fmt.Printf("re-evaluated %d profiles, %d changed level\n", re.Evaluated, re.Changed)
	return nil
}

// syncPointTypes upserts point types by slug. A primary type whose slug doesn't exist yet
// takes over the tenant's current primary type (renaming it), so a new tenant's default
// "points" type becomes, say, "coins" instead of lingering unused.
func syncPointTypes(c *client, types []pointType) error {
	if len(types) == 0 {
		return nil
	}
	var existing struct{ Data []pointType }
	if err := c.do("GET", "/admin/point-types", nil, &existing); err != nil {
		return err
	}
	bySlug := map[string]pointType{}
	var primary pointType
	for _, pt := range existing.Data {
		bySlug[pt.Slug] = pt
		if pt.IsPrimary {
			primary = pt
		}
	}
	for _, want := range types {
		if want.ExpiryMode == "" {
			want.ExpiryMode = "never"
		}
		if want.Limits == nil {
			want.Limits = []map[string]any{}
		}
		cur, ok := bySlug[want.Slug]
		if !ok && want.IsPrimary && primary.ID != "" {
			cur, ok = primary, true
		}
		if ok {
			if err := c.do("PATCH", "/admin/point-types/"+cur.ID, want, nil); err != nil {
				return err
			}
			fmt.Printf("point type %-9s updated\n", want.Slug)
			continue
		}
		if err := c.do("POST", "/admin/point-types", want, nil); err != nil {
			return err
		}
		fmt.Printf("point type %-9s created\n", want.Slug)
	}
	return nil
}

func syncLevels(c *client, levels []level, tiersBySlug map[string]tier) error {
	var existing struct{ Data []level }
	if err := c.do("GET", "/admin/levels", nil, &existing); err != nil {
		return err
	}
	bySlug := map[string]level{}
	for _, l := range existing.Data {
		bySlug[l.Slug] = l
	}
	for _, want := range levels {
		want.TierID = nil
		if want.Tier != "" {
			t, ok := tiersBySlug[want.Tier]
			if !ok {
				return fmt.Errorf("level %s: unknown tier %q", want.Slug, want.Tier)
			}
			want.TierID = &t.ID
		}
		want.Tier = ""
		if want.MinPoints == nil {
			want.MinPoints = map[string]int64{}
		}
		if cur, ok := bySlug[want.Slug]; ok {
			if err := c.do("PATCH", "/admin/levels/"+cur.ID, want, nil); err != nil {
				return err
			}
			fmt.Printf("level %-13s updated\n", want.Slug)
			continue
		}
		if err := c.do("POST", "/admin/levels", want, nil); err != nil {
			return err
		}
		fmt.Printf("level %-13s created\n", want.Slug)
	}
	return nil
}

func syncRules(c *client, tierID string, rules []rule) error {
	base := "/admin/tiers/" + tierID + "/rules"
	var existing struct{ Data []rule }
	if err := c.do("GET", base, nil, &existing); err != nil {
		return err
	}
	bySlug := map[string]rule{}
	for _, r := range existing.Data {
		bySlug[r.Slug] = r
	}
	for _, want := range rules {
		if cur, ok := bySlug[want.Slug]; ok {
			if err := c.do("PATCH", base+"/"+cur.ID, want, nil); err != nil {
				return err
			}
			fmt.Printf("  rule %-24s updated\n", want.Slug)
			continue
		}
		if err := c.do("POST", base, want, nil); err != nil {
			return err
		}
		fmt.Printf("  rule %-24s created\n", want.Slug)
	}
	return nil
}

// runDemo runs the demo transactions in order. A step whose idempotency key was already used
// replays its original result, so running the seed again changes nothing.
func runDemo(c *client, steps []demoStep) error {
	if len(steps) > 0 {
		fmt.Printf("demo: %d steps\n", len(steps))
	}
	for i, s := range steps {
		path, body := "/engine/execute", s.Execute
		if s.Adjust != nil {
			path, body = "/admin/adjustments", s.Adjust
		}
		var res struct {
			EntryType string `json:"entry_type"`
			Lines     []struct {
				PointType    string `json:"point_type"`
				Delta        int64  `json:"delta"`
				BalanceAfter int64  `json:"balance_after"`
			} `json:"lines"`
			LevelChange *struct {
				ToLevel *string `json:"to_level"`
			} `json:"level_change"`
			Streak *struct {
				Days int `json:"days"`
			} `json:"streak"`
			Replayed bool `json:"replayed"`
		}
		err := c.do("POST", path, body, &res)
		var ae *apiError
		switch {
		case s.Expect != "" && errors.As(err, &ae) && ae.Code == s.Expect:
			fmt.Printf("  %2d. %-58s -> %s (expected)\n", i+1, s.Note, ae.Code)
			continue
		case err != nil:
			return fmt.Errorf("demo step %d (%s): %w", i+1, s.Note, err)
		case s.Expect != "":
			// On a later run a daily cap or rate limit may have reset; that's not a failure.
			fmt.Printf("  %2d. %-58s -> succeeded (expected %s on a fresh run)\n", i+1, s.Note, s.Expect)
			continue
		}
		var parts []string
		for _, l := range res.Lines {
			parts = append(parts, fmt.Sprintf("%+d %s (=%d)", l.Delta, l.PointType, l.BalanceAfter))
		}
		if res.Streak != nil {
			parts = append(parts, fmt.Sprintf("streak day %d", res.Streak.Days))
		}
		if res.LevelChange != nil && res.LevelChange.ToLevel != nil {
			parts = append(parts, "now "+*res.LevelChange.ToLevel)
		}
		if res.Replayed {
			parts = append(parts, "replayed")
		}
		if len(parts) == 0 {
			parts = append(parts, "recorded")
		}
		fmt.Printf("  %2d. %-58s -> %s\n", i+1, s.Note, strings.Join(parts, ", "))
	}
	return nil
}
