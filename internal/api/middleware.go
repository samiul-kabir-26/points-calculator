package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxTenant
)

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

func tenantFrom(ctx context.Context) store.Tenant {
	t, _ := ctx.Value(ctxTenant).(store.Tenant)
	return t
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) withRequestContext(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || len(id) > 128 {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), ctxRequestID, id)
		r = r.WithContext(ctx)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				s.log.ErrorContext(ctx, "panic", "panic", p, "path", r.URL.Path)
				s.writeError(rec, r, errors.New("panic"))
			}
			s.log.InfoContext(ctx, "request",
				"method", r.Method, "path", r.URL.Path, "status", rec.status,
				"duration_ms", float64(time.Since(start).Microseconds())/1000, "request_id", id)
		}()
		next.ServeHTTP(rec, r)
	})
}

// requireTenant authenticates a tenant API key from "Authorization: Bearer <key>"
// or "X-API-Key: <key>" and stores the tenant in the request context.
func (s *Server) requireTenant(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key == "" {
			key = bearerToken(r)
		}
		if key == "" {
			s.writeError(w, r, apperr.New(http.StatusUnauthorized, "unauthenticated", "missing API key"))
			return
		}
		tenant, err := s.store.GetTenantByAPIKeyHash(r.Context(), hashAPIKey(key))
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, r, apperr.New(http.StatusUnauthorized, "unauthenticated", "invalid API key"))
			return
		}
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxTenant, tenant)))
	}
}

// requirePlatform guards tenant provisioning with the operator's platform token.
func (s *Server) requirePlatform(next http.HandlerFunc) http.HandlerFunc {
	want := sha256.Sum256([]byte(s.platformToken))
	return func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearerToken(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			s.writeError(w, r, apperr.New(http.StatusUnauthorized, "unauthenticated", "invalid platform token"))
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

const apiKeyPrefix = "pce_"

// newAPIKey returns a fresh secret key, its display prefix, and its hash. Keys
// carry 256 bits of entropy, so a fast unsalted hash is sufficient for storage.
func newAPIKey() (key, prefix string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", nil, err
	}
	key = apiKeyPrefix + base64.RawURLEncoding.EncodeToString(b)
	return key, key[:len(apiKeyPrefix)+8], hashAPIKey(key), nil
}

func hashAPIKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
