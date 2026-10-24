// Package store is the Postgres persistence layer. Every query is scoped by
// tenant_id; callers can never read or write across tenants through it.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound              = errors.New("not found")
	ErrConflict              = errors.New("unique constraint violated")
	ErrInsufficientBalance   = errors.New("balance would become negative")
	ErrBalanceOverflow       = errors.New("balance out of range")
	ErrDuplicateIdempotency  = errors.New("idempotency key already used")
	ErrArchitectureLocked    = errors.New("architecture key cannot change once profiles exist")
	ErrLastActiveAPIKey      = errors.New("cannot revoke the last active api key")
	ErrRuleNotFoundInTier    = errors.New("rule not found in tier")
	ErrSpendBelowZero        = errors.New("lifetime spend would become negative")
	ErrOrdersBelowZero       = errors.New("lifetime orders would become negative")
	ErrLevelRankTaken        = errors.New("another level already uses that rank")
	ErrLevelTierNotFound     = errors.New("level's tier not found")
	ErrPointTypeNotFound     = errors.New("point type not found")
	ErrPointTypeNotSpendable = errors.New("point type is not spendable")
	ErrPointTypeInUse        = errors.New("point type is in use")
	ErrPrimaryPointType      = errors.New("a tenant must have exactly one primary point type")
	ErrPointLimitExceeded    = errors.New("point limit exceeded")
	ErrStreakClaimed         = errors.New("streak already claimed for the current day")
	ErrDuplicateEffect       = errors.New("a rule can have only one effect per point type")
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type Tenant struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	ArchitectureKey *string   `json:"architecture_key"`
	// Timezone is an IANA name; day/week/month boundaries and month-end expiry follow it.
	Timezone    string      `json:"timezone"`
	LevelPolicy LevelPolicy `json:"level_policy"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

const (
	LevelBasisLifetime = "lifetime"
	LevelBasisRolling  = "rolling"
	LevelBasisCalendar = "calendar"
)

// LevelPolicy is a tenant's rule for turning qualifying activity (spend, orders, points
// earned) into a level.
type LevelPolicy struct {
	// Basis is LevelBasisLifetime (all activity counts), LevelBasisRolling (only the last
	// WindowDays) or LevelBasisCalendar (fixed periods of PeriodMonths, aligned to 1 January).
	Basis        string `json:"basis"`
	WindowDays   *int   `json:"window_days"`
	PeriodMonths *int   `json:"period_months"`
	// Downgrade lets an entity drop a level once it stops qualifying and GraceDays have passed.
	Downgrade bool `json:"downgrade"`
	GraceDays int  `json:"grace_days"`
	// MinTenureDays keeps a level from being lost before it has been held this long.
	MinTenureDays int `json:"min_tenure_days"`
}

const (
	ExpiryNever      = "never"
	ExpiryDays       = "days"
	ExpiryEndOfMonth = "end_of_month"
)

const (
	DirectionCredit = "credit"
	DirectionDebit  = "debit"
)

const (
	PeriodDay   = "day"
	PeriodWeek  = "week"
	PeriodMonth = "month"
)

// PointType is a currency the tenant defines, such as coins or XP.
type PointType struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"-"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	// IsPrimary marks the type the legacy single-balance fields refer to. Exactly one per tenant.
	IsPrimary bool `json:"is_primary"`
	// Spendable false means rules can only award this type.
	Spendable bool `json:"spendable"`
	// ExpiryMode and ExpiryValue are the default expiry of earned points.
	ExpiryMode  string       `json:"expiry_mode"`
	ExpiryValue *int         `json:"expiry_value"`
	Limits      []PointLimit `json:"limits"`
	CreatedAt   time.Time    `json:"created_at"`
	UpdatedAt   time.Time    `json:"updated_at"`
}

// PointLimit caps how much of a point type one entity can gain (credit) or spend (debit)
// through rules per calendar day, ISO week or month in the tenant's time zone.
type PointLimit struct {
	Direction string `json:"direction"`
	Period    string `json:"period"`
	Max       int64  `json:"max"`
}

const (
	EffectFixed    = "fixed"
	EffectPerSpend = "per_spend"
	EffectInput    = "input"
	EffectStreak   = "streak"
)

// Effect is what a rule does to one point type.
type Effect struct {
	// PointType is the type's slug; empty means the primary type when saving a rule.
	PointType   string    `json:"point_type"`
	PointTypeID uuid.UUID `json:"-"`
	// Kind is EffectFixed (Amount), EffectPerSpend (floor(spend * RatePoints / RatePer)),
	// EffectInput (the request's amount) or EffectStreak (Steps[day in streak - 1], cycling).
	Kind       string  `json:"kind"`
	Direction  string  `json:"direction"`
	Amount     int64   `json:"amount,omitempty"`
	RatePoints int64   `json:"rate_points,omitempty"`
	RatePer    int64   `json:"rate_per,omitempty"`
	Min        int64   `json:"min,omitempty"`
	Max        int64   `json:"max,omitempty"`
	Steps      []int64 `json:"steps,omitempty"`
	// ExpiryMode and ExpiryValue override the point type's default expiry for credits.
	ExpiryMode  *string `json:"expiry_mode,omitempty"`
	ExpiryValue *int    `json:"expiry_value,omitempty"`
}

type APIKey struct {
	ID        uuid.UUID  `json:"id"`
	TenantID  uuid.UUID  `json:"-"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

type Tier struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"-"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	IsActive    bool      `json:"is_active"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Rule struct {
	ID                     uuid.UUID `json:"id"`
	TenantID               uuid.UUID `json:"-"`
	TierID                 uuid.UUID `json:"tier_id"`
	Slug                   string    `json:"slug"`
	Name                   string    `json:"name"`
	Description            string    `json:"description"`
	Effects                []Effect  `json:"effects"`
	OrderDelta             int       `json:"order_delta"`
	CooldownSeconds        int       `json:"cooldown_seconds"`
	RateLimitMax           int       `json:"rate_limit_max"`
	RateLimitWindowSeconds int       `json:"rate_limit_window_seconds"`
	IsActive               bool      `json:"is_active"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

type Level struct {
	ID          uuid.UUID `json:"id"`
	TenantID    uuid.UUID `json:"-"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	// Rank orders the ladder; higher is better.
	Rank int `json:"rank"`
	// An entity qualifies when it meets every requirement below.
	MinSpend  int64            `json:"min_spend"`
	MinOrders int64            `json:"min_orders"`
	MinPoints map[string]int64 `json:"min_points"`
	TierID    *uuid.UUID       `json:"tier_id"`
	IsActive  bool             `json:"is_active"`
	CreatedAt time.Time        `json:"created_at"`
	UpdatedAt time.Time        `json:"updated_at"`
}

const (
	LevelUp   = "up"
	LevelDown = "down"
)

// LevelChange records an entity moving from one level to another.
type LevelChange struct {
	Sequence         int64            `json:"sequence"`
	EntityID         string           `json:"entity_id"`
	Direction        string           `json:"direction"`
	FromLevelID      *uuid.UUID       `json:"from_level_id"`
	FromLevelSlug    *string          `json:"from_level"`
	ToLevelID        *uuid.UUID       `json:"to_level_id"`
	ToLevelSlug      *string          `json:"to_level"`
	QualifyingSpend  int64            `json:"qualifying_spend"`
	QualifyingOrders int64            `json:"qualifying_orders"`
	QualifyingPoints map[string]int64 `json:"qualifying_points"`
	CreatedAt        time.Time        `json:"created_at"`
}

// ExecutableRule is a rule joined with the state of its tier, as needed by the engine.
type ExecutableRule struct {
	Rule
	TierSlug     string
	TierIsActive bool
}

type Profile struct {
	EntityID string `json:"entity_id"`
	// Balance is the balance of the tenant's primary point type.
	Balance int64 `json:"balance"`
	// Balances holds every point type the entity has touched, keyed by slug.
	Balances         map[string]Balance `json:"balances"`
	TierID           *uuid.UUID         `json:"tier_id"`
	TierSlug         *string            `json:"tier_slug"`
	LifetimeSpend    int64              `json:"lifetime_spend"`
	QualifyingSpend  int64              `json:"qualifying_spend"`
	LifetimeOrders   int64              `json:"lifetime_orders"`
	QualifyingOrders int64              `json:"qualifying_orders"`
	LevelID          *uuid.UUID         `json:"level_id"`
	LevelSlug        *string            `json:"level_slug"`
	LevelSince       *time.Time         `json:"level_since"`
	// BelowLevelSince is set while the entity no longer meets its level's threshold.
	BelowLevelSince *time.Time `json:"below_level_since"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Balance is an entity's holding of one point type.
type Balance struct {
	Balance int64 `json:"balance"`
	// LifetimeEarned counts rule credits and adjustments; redemptions and expiry don't reduce it.
	LifetimeEarned int64 `json:"lifetime_earned"`
	// QualifyingEarned is LifetimeEarned within the level basis window, as of the last evaluation.
	QualifyingEarned int64 `json:"qualifying_earned"`
}

// Expiration is a lot of points that expire together.
type Expiration struct {
	PointType string    `json:"point_type"`
	Amount    int64     `json:"amount"`
	ExpiresAt time.Time `json:"expires_at"`
}

const (
	EntryTypeRule       = "rule"
	EntryTypeAdjustment = "adjustment"
	EntryTypeExpiry     = "expiry"
)

// LedgerLine is the movement of one point type within a ledger entry.
type LedgerLine struct {
	PointType     string    `json:"point_type"`
	PointTypeID   uuid.UUID `json:"point_type_id"`
	Delta         int64     `json:"delta"`
	BalanceBefore int64     `json:"balance_before"`
	BalanceAfter  int64     `json:"balance_after"`
}

type LedgerEntry struct {
	Sequence      int64      `json:"sequence"`
	TransactionID uuid.UUID  `json:"transaction_id"`
	EntityID      string     `json:"entity_id"`
	EntryType     string     `json:"entry_type"`
	TierID        *uuid.UUID `json:"tier_id"`
	TierSlug      *string    `json:"tier"`
	RuleID        *uuid.UUID `json:"rule_id"`
	RuleSlug      *string    `json:"rule"`
	// PointDelta, BalanceBefore and BalanceAfter summarise the primary point type; Lines
	// has every type the entry moved.
	PointDelta    int64        `json:"point_delta"`
	BalanceBefore int64        `json:"balance_before"`
	BalanceAfter  int64        `json:"balance_after"`
	Lines         []LedgerLine `json:"lines"`
	Spend         int64        `json:"spend"`
	Orders        int64        `json:"orders"`
	// InputAmount is the caller-supplied amount for rules with an input effect.
	InputAmount    *int64    `json:"amount"`
	IdempotencyKey *string   `json:"idempotency_key"`
	Reason         *string   `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}

// Postgres error codes we translate into domain errors.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
	pgNumericOutOfRange   = "22003"
)

func pgErr(err error) (*pgconn.PgError, bool) {
	var e *pgconn.PgError
	ok := errors.As(err, &e)
	return e, ok
}

// translate maps common driver errors onto store errors.
func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if e, ok := pgErr(err); ok {
		switch e.Code {
		case pgUniqueViolation:
			return ErrConflict
		case pgForeignKeyViolation:
			return ErrNotFound
		}
	}
	return err
}

// querier is satisfied by both the pool and a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BalanceError reports which point type a deduction couldn't be afforded in.
type BalanceError struct{ PointType string }

func (e *BalanceError) Error() string { return "insufficient " + e.PointType + " balance" }
func (e *BalanceError) Unwrap() error { return ErrInsufficientBalance }

// LimitError reports a debit that would exceed a point type's per-period cap.
type LimitError struct {
	PointType string
	Limit     PointLimit
	// Remaining is how much of the allowance is left in the current period.
	Remaining int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("%s %s limit of %d per %s exceeded", e.PointType, e.Limit.Direction, e.Limit.Max, e.Limit.Period)
}
func (e *LimitError) Unwrap() error { return ErrPointLimitExceeded }

// StreakError reports a second streak claim on the same local day.
type StreakError struct{ RetryAfter time.Duration }

func (e *StreakError) Error() string { return ErrStreakClaimed.Error() }
func (e *StreakError) Unwrap() error { return ErrStreakClaimed }

// PointTypeError names the point type a rule, level or adjustment referred to.
type PointTypeError struct {
	Slug string
	Err  error
}

func (e *PointTypeError) Error() string { return fmt.Sprintf("point type %q: %v", e.Slug, e.Err) }
func (e *PointTypeError) Unwrap() error { return e.Err }
