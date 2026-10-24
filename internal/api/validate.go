package api

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// Architecture keys are field names like user_id or walletAddress.
	archKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
)

const (
	maxNameLen        = 200
	maxDescriptionLen = 2000
	maxEntityIDLen    = 255
	maxIdemKeyLen     = 255
	maxReasonLen      = 1000
	// maxSpend bounds a single spend or threshold so sums can't overflow.
	maxSpend = 1_000_000_000_000_000
	// maxPoints bounds a single amount, cap or threshold of points.
	maxPoints = 1_000_000_000_000_000
	// maxOrders bounds order thresholds and corrections.
	maxOrders = 1_000_000_000
	// maxRank bounds level ranks.
	maxRank = 1_000_000
	// maxExpiryMonths caps end_of_month expiry at ten years.
	maxExpiryMonths = 120
	// maxEffects, maxStreakSteps, maxLimits and maxRequirements keep configurations small.
	maxEffects      = 10
	maxStreakSteps  = 366
	maxLimits       = 6
	maxRequirements = 10
	// maxPolicyDays caps level windows and grace periods at ten years.
	maxPolicyDays = 3650
	// maxLimitSeconds caps cooldowns and throttle windows at one year.
	maxLimitSeconds = 365 * 24 * 60 * 60
)

// fieldErrors accumulates validation failures into a single 400 response.
type fieldErrors []string

func (f *fieldErrors) add(msg string) { *f = append(*f, msg) }

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return apperr.New(http.StatusBadRequest, "validation_failed", "%s", strings.Join(f, "; "))
}

func (f *fieldErrors) slug(field, v string) {
	if !slugPattern.MatchString(v) {
		f.add(field + " must be 1-64 chars of lowercase letters, digits, '_' or '-', starting with a letter or digit")
	}
}

func (f *fieldErrors) text(field, v string, required bool, max int) {
	switch {
	case required && strings.TrimSpace(v) == "":
		f.add(field + " is required")
	case utf8.RuneCountInString(v) > max:
		f.add(field + " is too long")
	}
}

func validateTier(t store.Tier) error {
	var f fieldErrors
	f.slug("slug", t.Slug)
	f.text("name", t.Name, true, maxNameLen)
	f.text("description", t.Description, false, maxDescriptionLen)
	return f.err()
}

func validateRule(r store.Rule) error {
	var f fieldErrors
	f.slug("slug", r.Slug)
	f.text("name", r.Name, true, maxNameLen)
	f.text("description", r.Description, false, maxDescriptionLen)
	if r.OrderDelta < -1000 || r.OrderDelta > 1000 {
		f.add("order_delta must be between -1000 and 1000")
	}
	if len(r.Effects) > maxEffects {
		f.add(fmt.Sprintf("a rule can have at most %d effects", maxEffects))
	}
	seen := map[string]bool{}
	inputs := 0
	for i, e := range r.Effects {
		validateEffect(&f, fmt.Sprintf("effects[%d]", i), e)
		if seen[e.PointType] {
			f.add(fmt.Sprintf("effects[%d]: a rule can have only one effect per point type", i))
		}
		seen[e.PointType] = true
		if e.Kind == store.EffectInput {
			inputs++
		}
	}
	if inputs > 1 {
		f.add("a rule can have at most one input effect")
	}
	if r.CooldownSeconds < 0 || r.CooldownSeconds > maxLimitSeconds {
		f.add("cooldown_seconds must be between 0 and one year")
	}
	if r.RateLimitMax < 0 {
		f.add("rate_limit_max must be >= 0")
	}
	if r.RateLimitWindowSeconds < 0 || r.RateLimitWindowSeconds > maxLimitSeconds {
		f.add("rate_limit_window_seconds must be between 0 and one year")
	}
	if (r.RateLimitMax == 0) != (r.RateLimitWindowSeconds == 0) {
		f.add("rate_limit_max and rate_limit_window_seconds must be set together (both 0 disables throttling)")
	}
	return f.err()
}

// validateEffect checks one rule effect. PointType "" (the primary type) is allowed.
func validateEffect(f *fieldErrors, field string, e store.Effect) {
	if e.PointType != "" && !slugPattern.MatchString(e.PointType) {
		f.add(field + ".point_type must be a point type slug")
	}
	if e.Direction != store.DirectionCredit && e.Direction != store.DirectionDebit {
		f.add(field + ".direction must be credit or debit")
	}
	positive := func(name string, v, max int64) {
		if v < 1 || v > max {
			f.add(fmt.Sprintf("%s.%s must be between 1 and %d", field, name, max))
		}
	}
	unused := func(name string, set bool) {
		if set {
			f.add(fmt.Sprintf("%s.%s doesn't apply to %s effects", field, name, e.Kind))
		}
	}
	switch e.Kind {
	case store.EffectFixed:
		positive("amount", e.Amount, maxPoints)
		unused("rate_points", e.RatePoints != 0)
		unused("rate_per", e.RatePer != 0)
		unused("steps", len(e.Steps) > 0)
		unused("max", e.Max != 0)
	case store.EffectPerSpend:
		positive("rate_points", e.RatePoints, maxPoints)
		positive("rate_per", e.RatePer, maxSpend)
		unused("amount", e.Amount != 0)
		unused("steps", len(e.Steps) > 0)
		if e.Direction == store.DirectionDebit {
			f.add(field + ": per_spend effects can only credit")
		}
	case store.EffectInput:
		unused("amount", e.Amount != 0)
		unused("rate_points", e.RatePoints != 0)
		unused("rate_per", e.RatePer != 0)
		unused("steps", len(e.Steps) > 0)
		if e.Max > 0 && e.Min > e.Max {
			f.add(field + ".min must not exceed max")
		}
	case store.EffectStreak:
		if len(e.Steps) == 0 || len(e.Steps) > maxStreakSteps {
			f.add(fmt.Sprintf("%s.steps must have 1-%d amounts", field, maxStreakSteps))
		}
		for _, s := range e.Steps {
			if s < 1 || s > maxPoints {
				f.add(field + ".steps must all be positive")
				break
			}
		}
		unused("amount", e.Amount != 0)
		unused("rate_points", e.RatePoints != 0)
		unused("rate_per", e.RatePer != 0)
		unused("max", e.Max != 0)
		if e.Direction == store.DirectionDebit {
			f.add(field + ": streak effects can only credit")
		}
	default:
		f.add(field + ".kind must be fixed, per_spend, input or streak")
	}
	if e.Kind != store.EffectInput && e.Min != 0 {
		f.add(field + ".min applies only to input effects")
	}
	if e.Min < 0 || e.Min > maxPoints || e.Max < 0 || e.Max > maxPoints {
		f.add(fmt.Sprintf("%s.min and max must be between 0 and %d", field, int64(maxPoints)))
	}
	if e.ExpiryMode != nil || e.ExpiryValue != nil {
		if e.Direction == store.DirectionDebit {
			f.add(field + ": expiry applies only to credits")
		}
		if e.ExpiryMode == nil {
			f.add(field + ".expiry_mode is required with expiry_value")
		} else {
			validateExpiry(f, field+".", *e.ExpiryMode, e.ExpiryValue)
		}
	}
}

// validateExpiry checks an expiry mode and value pair; prefix qualifies the field names.
func validateExpiry(f *fieldErrors, prefix, mode string, value *int) {
	switch mode {
	case store.ExpiryNever:
		if value != nil {
			f.add(prefix + "expiry_value must be omitted when expiry_mode is never")
		}
	case store.ExpiryDays:
		if value == nil || *value < 1 || *value > maxPolicyDays {
			f.add(prefix + "expiry_value must be 1-3650 days")
		}
	case store.ExpiryEndOfMonth:
		if value == nil || *value < 1 || *value > maxExpiryMonths {
			f.add(fmt.Sprintf("%sexpiry_value must be 1-%d months", prefix, maxExpiryMonths))
		}
	default:
		f.add(prefix + "expiry_mode must be never, days or end_of_month")
	}
}

func validatePointType(pt store.PointType) error {
	var f fieldErrors
	f.slug("slug", pt.Slug)
	f.text("name", pt.Name, true, maxNameLen)
	f.text("description", pt.Description, false, maxDescriptionLen)
	validateExpiry(&f, "", pt.ExpiryMode, pt.ExpiryValue)
	if len(pt.Limits) > maxLimits {
		f.add(fmt.Sprintf("a point type can have at most %d limits", maxLimits))
	}
	seen := map[string]bool{}
	for i, l := range pt.Limits {
		field := fmt.Sprintf("limits[%d]", i)
		if l.Direction != store.DirectionCredit && l.Direction != store.DirectionDebit {
			f.add(field + ".direction must be credit or debit")
		}
		if l.Period != store.PeriodDay && l.Period != store.PeriodWeek && l.Period != store.PeriodMonth {
			f.add(field + ".period must be day, week or month")
		}
		if l.Max < 1 || l.Max > maxPoints {
			f.add(field + ".max must be positive")
		}
		if seen[l.Direction+"/"+l.Period] {
			f.add(field + ": only one limit per direction and period")
		}
		seen[l.Direction+"/"+l.Period] = true
	}
	return f.err()
}

func validateLevel(l store.Level) error {
	var f fieldErrors
	f.slug("slug", l.Slug)
	f.text("name", l.Name, true, maxNameLen)
	f.text("description", l.Description, false, maxDescriptionLen)
	if l.Rank < 0 || l.Rank > maxRank {
		f.add(fmt.Sprintf("rank must be between 0 and %d", maxRank))
	}
	if l.MinSpend < 0 || l.MinSpend > maxSpend {
		f.add("min_spend must be between 0 and 10^15")
	}
	if l.MinOrders < 0 || l.MinOrders > maxOrders {
		f.add("min_orders must be between 0 and 10^9")
	}
	if len(l.MinPoints) > maxRequirements {
		f.add(fmt.Sprintf("min_points can name at most %d point types", maxRequirements))
	}
	for slug, n := range l.MinPoints {
		if !slugPattern.MatchString(slug) {
			f.add("min_points keys must be point type slugs")
		}
		if n < 0 || n > maxPoints {
			f.add("min_points values must be between 0 and 10^15")
		}
	}
	return f.err()
}

func validateLevelPolicy(p store.LevelPolicy) error {
	var f fieldErrors
	switch p.Basis {
	case store.LevelBasisLifetime:
		if p.WindowDays != nil || p.PeriodMonths != nil {
			f.add("window_days and period_months don't apply to the lifetime basis")
		}
	case store.LevelBasisRolling:
		if p.WindowDays == nil || *p.WindowDays < 1 || *p.WindowDays > maxPolicyDays {
			f.add("window_days is required for the rolling basis and must be between 1 and 3650")
		}
		if p.PeriodMonths != nil {
			f.add("period_months applies only to the calendar basis")
		}
	case store.LevelBasisCalendar:
		if p.PeriodMonths == nil || *p.PeriodMonths < 1 || 12%*p.PeriodMonths != 0 {
			f.add("period_months is required for the calendar basis and must be 1, 2, 3, 4, 6 or 12")
		}
		if p.WindowDays != nil {
			f.add("window_days applies only to the rolling basis")
		}
	default:
		f.add("basis must be lifetime, rolling or calendar")
	}
	if p.GraceDays < 0 || p.GraceDays > maxPolicyDays {
		f.add("grace_days must be between 0 and 3650")
	}
	if p.MinTenureDays < 0 || p.MinTenureDays > maxPolicyDays {
		f.add("min_tenure_days must be between 0 and 3650")
	}
	return f.err()
}

func validateTimezone(f *fieldErrors, v string) {
	if _, err := store.LoadLocation(v); err != nil {
		f.add("timezone must be an IANA time zone name such as Asia/Dhaka")
	}
}

func validateSpend(f *fieldErrors, v int64) {
	if v < -maxSpend || v > maxSpend {
		f.add("spend must be between -10^15 and 10^15")
	}
}

func validateEntityID(f *fieldErrors, field, v string) {
	switch {
	case v == "":
		f.add(field + " is required")
	case len(v) > maxEntityIDLen:
		f.add(field + " is too long")
	case !utf8.ValidString(v):
		f.add(field + " must be valid UTF-8")
	}
}

func validateIdempotencyKey(f *fieldErrors, v string) {
	if len(v) > maxIdemKeyLen {
		f.add("idempotency_key is too long")
	}
}
