package api

import (
	"encoding/json"
	"testing"

	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

func fixed(amount int64) store.Effect {
	return store.Effect{Kind: store.EffectFixed, Direction: store.DirectionCredit, Amount: amount}
}

func TestValidateRule(t *testing.T) {
	valid := store.Rule{Slug: "daily_login", Name: "Daily login", Effects: []store.Effect{fixed(10)}}
	cases := []struct {
		name   string
		mutate func(*store.Rule)
		ok     bool
	}{
		{"valid", func(*store.Rule) {}, true},
		{"debit", func(r *store.Rule) { r.Effects[0].Direction = store.DirectionDebit }, true},
		{"no effects", func(r *store.Rule) { r.Effects = nil; r.OrderDelta = 1 }, true},
		{"throttle pair", func(r *store.Rule) { r.RateLimitMax, r.RateLimitWindowSeconds = 5, 86400 }, true},
		{"zero amount", func(r *store.Rule) { r.Effects[0].Amount = 0 }, false},
		{"order delta too big", func(r *store.Rule) { r.OrderDelta = 5000 }, false},
		{"two effects on one type", func(r *store.Rule) { r.Effects = append(r.Effects, fixed(5)) }, false},
		{"two input effects", func(r *store.Rule) {
			r.Effects = []store.Effect{
				{PointType: "coins", Kind: store.EffectInput, Direction: store.DirectionDebit},
				{PointType: "gems", Kind: store.EffectInput, Direction: store.DirectionDebit},
			}
		}, false},
		{"uppercase slug", func(r *store.Rule) { r.Slug = "Daily" }, false},
		{"slug with space", func(r *store.Rule) { r.Slug = "daily login" }, false},
		{"missing name", func(r *store.Rule) { r.Name = " " }, false},
		{"negative cooldown", func(r *store.Rule) { r.CooldownSeconds = -1 }, false},
		{"max without window", func(r *store.Rule) { r.RateLimitMax = 5 }, false},
		{"window without max", func(r *store.Rule) { r.RateLimitWindowSeconds = 60 }, false},
		{"window too long", func(r *store.Rule) { r.RateLimitMax, r.RateLimitWindowSeconds = 1, maxLimitSeconds+1 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid
			r.Effects = append([]store.Effect(nil), valid.Effects...)
			tc.mutate(&r)
			if err := validateRule(r); (err == nil) != tc.ok {
				t.Fatalf("validateRule() error = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestValidateEffect(t *testing.T) {
	days, months, tooMany := 30, 3, 121
	never, inDays, monthEnd := store.ExpiryNever, store.ExpiryDays, store.ExpiryEndOfMonth
	cases := []struct {
		name string
		e    store.Effect
		ok   bool
	}{
		{"per spend", store.Effect{Kind: store.EffectPerSpend, Direction: "credit", RatePoints: 1, RatePer: 100, Max: 2000}, true},
		{"per spend without rate", store.Effect{Kind: store.EffectPerSpend, Direction: "credit", RatePoints: 1}, false},
		{"per spend debit", store.Effect{Kind: store.EffectPerSpend, Direction: "debit", RatePoints: 1, RatePer: 1}, false},
		{"input with bounds", store.Effect{Kind: store.EffectInput, Direction: "debit", Min: 100, Max: 30000}, true},
		{"input min above max", store.Effect{Kind: store.EffectInput, Direction: "debit", Min: 500, Max: 100}, false},
		{"streak", store.Effect{Kind: store.EffectStreak, Direction: "credit", Steps: []int64{5, 5, 10, 10, 15, 15, 30}}, true},
		{"streak without steps", store.Effect{Kind: store.EffectStreak, Direction: "credit"}, false},
		{"streak with zero step", store.Effect{Kind: store.EffectStreak, Direction: "credit", Steps: []int64{5, 0}}, false},
		{"fixed with rate", store.Effect{Kind: store.EffectFixed, Direction: "credit", Amount: 1, RatePer: 5}, false},
		{"min on fixed", store.Effect{Kind: store.EffectFixed, Direction: "credit", Amount: 1, Min: 1}, false},
		{"unknown kind", store.Effect{Kind: "bonus", Direction: "credit"}, false},
		{"bad direction", store.Effect{Kind: store.EffectFixed, Direction: "up", Amount: 1}, false},
		{"bad point type", store.Effect{PointType: "Coins!", Kind: store.EffectFixed, Direction: "credit", Amount: 1}, false},
		{"expiry override days", store.Effect{Kind: store.EffectFixed, Direction: "credit", Amount: 1, ExpiryMode: &inDays, ExpiryValue: &days}, true},
		{"expiry override never", store.Effect{Kind: store.EffectFixed, Direction: "credit", Amount: 1, ExpiryMode: &never}, true},
		{"expiry months too long", store.Effect{Kind: store.EffectFixed, Direction: "credit", Amount: 1, ExpiryMode: &monthEnd, ExpiryValue: &tooMany}, false},
		{"expiry value without mode", store.Effect{Kind: store.EffectFixed, Direction: "credit", Amount: 1, ExpiryValue: &months}, false},
		{"expiry on a debit", store.Effect{Kind: store.EffectFixed, Direction: "debit", Amount: 1, ExpiryMode: &never}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f fieldErrors
			validateEffect(&f, "effects[0]", tc.e)
			if (f.err() == nil) != tc.ok {
				t.Fatalf("validateEffect() = %v, want ok=%v", f.err(), tc.ok)
			}
		})
	}
}

func TestValidatePointType(t *testing.T) {
	three := 3
	valid := store.PointType{Slug: "coins", Name: "Coins", ExpiryMode: store.ExpiryEndOfMonth, ExpiryValue: &three,
		Limits: []store.PointLimit{{Direction: "debit", Period: "day", Max: 30000}, {Direction: "debit", Period: "week", Max: 45000}}}
	cases := []struct {
		name   string
		mutate func(*store.PointType)
		ok     bool
	}{
		{"valid", func(*store.PointType) {}, true},
		{"never with value", func(p *store.PointType) { p.ExpiryMode = store.ExpiryNever }, false},
		{"never", func(p *store.PointType) { p.ExpiryMode, p.ExpiryValue = store.ExpiryNever, nil }, true},
		{"duplicate limit", func(p *store.PointType) { p.Limits = append(p.Limits, p.Limits[0]) }, false},
		{"bad period", func(p *store.PointType) { p.Limits[0].Period = "year" }, false},
		{"zero max", func(p *store.PointType) { p.Limits[0].Max = 0 }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			p.Limits = append([]store.PointLimit(nil), valid.Limits...)
			tc.mutate(&p)
			if err := validatePointType(p); (err == nil) != tc.ok {
				t.Fatalf("validatePointType() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestValidateLevelPolicy(t *testing.T) {
	six, five, thirty := 6, 5, 30
	cases := []struct {
		name string
		p    store.LevelPolicy
		ok   bool
	}{
		{"lifetime", store.LevelPolicy{Basis: "lifetime"}, true},
		{"rolling", store.LevelPolicy{Basis: "rolling", WindowDays: &thirty}, true},
		{"calendar", store.LevelPolicy{Basis: "calendar", PeriodMonths: &six, Downgrade: true}, true},
		{"calendar period not dividing 12", store.LevelPolicy{Basis: "calendar", PeriodMonths: &five}, false},
		{"calendar without period", store.LevelPolicy{Basis: "calendar"}, false},
		{"calendar with window", store.LevelPolicy{Basis: "calendar", PeriodMonths: &six, WindowDays: &thirty}, false},
		{"rolling with period", store.LevelPolicy{Basis: "rolling", WindowDays: &thirty, PeriodMonths: &six}, false},
		{"negative tenure", store.LevelPolicy{Basis: "lifetime", MinTenureDays: -1}, false},
	}
	for _, tc := range cases {
		if err := validateLevelPolicy(tc.p); (err == nil) != tc.ok {
			t.Errorf("%s: validateLevelPolicy() = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestScalarString(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{`"user123"`, "user123", false},
		{`12345678901234567890`, "12345678901234567890", false},
		{`null`, "", false},
		{``, "", false},
		{`{"a":1}`, "", true},
		{`true`, "", true},
	}
	for _, tc := range cases {
		got, err := scalarString(json.RawMessage(tc.in))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("scalarString(%s) = %q, %v; want %q, err=%v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}
