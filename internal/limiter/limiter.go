// Package limiter enforces per-entity rule cooldowns and sliding-window rate limits
// in Redis, so limits hold across every stateless API instance.
package limiter

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Outcome of a reservation attempt.
type Outcome int

const (
	Allowed Outcome = iota
	CooldownActive
	RateLimited
)

type Decision struct {
	Outcome    Outcome
	RetryAfter time.Duration
}

type Limits struct {
	Cooldown time.Duration
	MaxHits  int
	Window   time.Duration
}

func (l Limits) none() bool { return l.Cooldown <= 0 && (l.MaxHits <= 0 || l.Window <= 0) }

// Reservation identifies the slot taken by an allowed request so it can be
// returned if the request ultimately fails.
type Reservation struct {
	cooldownKey, windowKey, token string
}

type Limiter struct {
	rdb redis.UniversalClient
}

func New(rdb redis.UniversalClient) *Limiter { return &Limiter{rdb: rdb} }

// reserveScript checks the cooldown and sliding window and, if both pass, records
// the hit, all atomically. Time comes from the Redis server so every API instance
// shares one clock.
var reserveScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local cooldown = tonumber(ARGV[1])
local limit = tonumber(ARGV[2])
local window = tonumber(ARGV[3])

if cooldown > 0 then
  local ttl = redis.call('PTTL', KEYS[1])
  if ttl > 0 then return {1, ttl} end
end

if limit > 0 then
  redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now - window)
  if redis.call('ZCARD', KEYS[2]) >= limit then
    local oldest = redis.call('ZRANGE', KEYS[2], 0, 0, 'WITHSCORES')
    local retry = tonumber(oldest[2]) + window - now
    if retry < 1 then retry = 1 end
    return {2, retry}
  end
end

if cooldown > 0 then redis.call('SET', KEYS[1], ARGV[4], 'PX', cooldown) end
if limit > 0 then
  redis.call('ZADD', KEYS[2], now, ARGV[4])
  redis.call('PEXPIRE', KEYS[2], window)
end
return {0, 0}
`)

// releaseScript undoes a reservation, touching the cooldown only if it is still ours.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then redis.call('DEL', KEYS[1]) end
redis.call('ZREM', KEYS[2], ARGV[1])
return 0
`)

// Reserve attempts to take a slot for one execution of ruleID by entityID.
// token must be unique per request.
func (l *Limiter) Reserve(ctx context.Context, tenantID, ruleID, entityID, token string, lim Limits) (Decision, *Reservation, error) {
	if lim.none() {
		return Decision{Outcome: Allowed}, nil, nil
	}
	maxHits, window := lim.MaxHits, lim.Window
	if maxHits <= 0 || window <= 0 {
		maxHits, window = 0, 0
	}
	// The hash tag keeps both keys in one Redis Cluster slot, as Lua scripts require.
	tag := fmt.Sprintf("{%s:%s:%s}", tenantID, ruleID, entityID)
	res := &Reservation{cooldownKey: "pc:cd:" + tag, windowKey: "pc:rl:" + tag, token: token}

	vals, err := reserveScript.Run(ctx, l.rdb, []string{res.cooldownKey, res.windowKey},
		lim.Cooldown.Milliseconds(), maxHits, window.Milliseconds(), token).Int64Slice()
	if err != nil {
		return Decision{}, nil, fmt.Errorf("limiter reserve: %w", err)
	}
	d := Decision{Outcome: Outcome(vals[0]), RetryAfter: time.Duration(vals[1]) * time.Millisecond}
	if d.Outcome != Allowed {
		return d, nil, nil
	}
	return d, res, nil
}

// Release returns a reservation's slot. It is a no-op for a nil reservation.
func (l *Limiter) Release(ctx context.Context, r *Reservation) error {
	if r == nil {
		return nil
	}
	return releaseScript.Run(ctx, l.rdb, []string{r.cooldownKey, r.windowKey}, r.token).Err()
}

// Lock is a best-effort mutual exclusion on key with a TTL, used to stop
// concurrent requests carrying the same idempotency key from racing.
type Lock struct {
	key, token string
}

var unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

// TryLock returns nil if the lock is already held.
func (l *Limiter) TryLock(ctx context.Context, key, token string, ttl time.Duration) (*Lock, error) {
	ok, err := l.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, fmt.Errorf("limiter lock: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return &Lock{key: key, token: token}, nil
}

func (l *Limiter) Unlock(ctx context.Context, lk *Lock) error {
	if lk == nil {
		return nil
	}
	return unlockScript.Run(ctx, l.rdb, []string{lk.key}, lk.token).Err()
}

func (l *Limiter) Ping(ctx context.Context) error { return l.rdb.Ping(ctx).Err() }
