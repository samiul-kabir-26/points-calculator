package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

type profileResponse struct {
	ArchitectureKey *string `json:"architecture_key"`
	store.Profile
}

type profilePage struct {
	ArchitectureKey *string `json:"architecture_key"`
	page[store.Profile]
}

func (s *Server) getProfile(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	p, err := s.store.GetProfile(r.Context(), tenant.ID, r.PathValue("entity_id"))
	if errors.Is(err, store.ErrNotFound) {
		err = apperr.NotFound("profile_not_found", "no profile exists for that entity")
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, profileResponse{ArchitectureKey: tenant.ArchitectureKey, Profile: p})
}

// listProfiles handles GET /data/profiles?tier=&level=&limit=&cursor=.
func (s *Server) listProfiles(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	limit, err := parseLimit(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f := store.ProfileFilter{TierSlug: queryPtr(r, "tier"), LevelSlug: queryPtr(r, "level"), Limit: limit + 1}
	if c := r.URL.Query().Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil {
			s.writeError(w, r, invalidCursor())
			return
		}
		after := string(b)
		f.After = &after
	}
	profiles, err := s.store.ListProfiles(r.Context(), tenant.ID, f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := profilePage{ArchitectureKey: tenant.ArchitectureKey, page: page[store.Profile]{Data: profiles}}
	if len(profiles) > limit {
		out.Data = profiles[:limit]
		c := base64.RawURLEncoding.EncodeToString([]byte(profiles[limit-1].EntityID))
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
}

// listEntityLogs handles GET /data/profiles/{entity_id}/logs with an optional point_type filter.
func (s *Server) listEntityLogs(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	entityID := r.PathValue("entity_id")
	if _, err := s.store.GetProfile(r.Context(), tenant.ID, entityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = apperr.NotFound("profile_not_found", "no profile exists for that entity")
		}
		s.writeError(w, r, err)
		return
	}
	s.writeLogs(w, r, func(f *store.LedgerFilter) {
		f.EntityID = &entityID
		f.PointType = queryPtr(r, "point_type")
	})
}

// listLogs handles GET /data/logs with optional filters
// from, to, tier, rule, entity_id, entry_type and point_type.
func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	from, err := parseTime(r, "from")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	to, err := parseTime(r, "to")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	entryType := queryPtr(r, "entry_type")
	if entryType != nil && *entryType != store.EntryTypeRule && *entryType != store.EntryTypeAdjustment &&
		*entryType != store.EntryTypeExpiry {
		s.writeError(w, r, apperr.BadRequest("entry_type must be rule, adjustment or expiry"))
		return
	}
	s.writeLogs(w, r, func(f *store.LedgerFilter) {
		f.From, f.To = from, to
		f.TierSlug = queryPtr(r, "tier")
		f.RuleSlug = queryPtr(r, "rule")
		f.EntityID = queryPtr(r, "entity_id")
		f.EntryType = entryType
		f.PointType = queryPtr(r, "point_type")
	})
}

// listExpirations handles GET /data/profiles/{entity_id}/expirations: the entity's points
// that are due to expire, soonest first.
func (s *Server) listExpirations(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	entityID := r.PathValue("entity_id")
	if _, err := s.store.GetProfile(r.Context(), tenant.ID, entityID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			err = apperr.NotFound("profile_not_found", "no profile exists for that entity")
		}
		s.writeError(w, r, err)
		return
	}
	exp, err := s.store.ListExpirations(r.Context(), tenant.ID, entityID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": exp})
}

// writeLogs applies the shared pagination parameters (limit, cursor, order) and renders a page.
func (s *Server) writeLogs(w http.ResponseWriter, r *http.Request, filter func(*store.LedgerFilter)) {
	limit, err := parseLimit(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	asc, err := parseOrder(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f := store.LedgerFilter{Limit: limit + 1, Ascending: asc}
	if c := r.URL.Query().Get("cursor"); c != "" {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			s.writeError(w, r, invalidCursor())
			return
		}
		f.Cursor = &n
	}
	filter(&f)
	entries, err := s.store.ListEntries(r.Context(), tenantFrom(r.Context()).ID, f)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := page[store.LedgerEntry]{Data: entries}
	if len(entries) > limit {
		out.Data = entries[:limit]
		out.NextCursor = cursorString(entries[limit-1].Sequence)
	}
	writeJSON(w, http.StatusOK, out)
}
