package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

// nullableInt tells an absent field (Set false) apart from an explicit null.
type nullableInt struct {
	Set   bool
	Value *int
}

func (n *nullableInt) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

type pointTypeRequest struct {
	Slug        *string             `json:"slug"`
	Name        *string             `json:"name"`
	Description *string             `json:"description"`
	IsPrimary   *bool               `json:"is_primary"`
	Spendable   *bool               `json:"spendable"`
	ExpiryMode  *string             `json:"expiry_mode"`
	ExpiryValue nullableInt         `json:"expiry_value"`
	Limits      *[]store.PointLimit `json:"limits"`
}

func (req pointTypeRequest) apply(pt *store.PointType) {
	set(&pt.Slug, req.Slug)
	set(&pt.Name, req.Name)
	set(&pt.Description, req.Description)
	set(&pt.IsPrimary, req.IsPrimary)
	set(&pt.Spendable, req.Spendable)
	if req.ExpiryMode != nil {
		pt.ExpiryMode = *req.ExpiryMode
		// Switching to never drops the value instead of making the caller clear it.
		if pt.ExpiryMode == store.ExpiryNever && !req.ExpiryValue.Set {
			pt.ExpiryValue = nil
		}
	}
	if req.ExpiryValue.Set {
		pt.ExpiryValue = req.ExpiryValue.Value
	}
	if req.Limits != nil {
		pt.Limits = *req.Limits
	}
}

func (s *Server) createPointType(w http.ResponseWriter, r *http.Request) {
	var req pointTypeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	pt := store.PointType{
		TenantID: tenantFrom(r.Context()).ID, Spendable: true, ExpiryMode: store.ExpiryNever,
		Limits: []store.PointLimit{},
	}
	req.apply(&pt)
	if err := validatePointType(pt); err != nil {
		s.writeError(w, r, err)
		return
	}
	pt, err := s.store.CreatePointType(r.Context(), pt)
	if err != nil {
		s.writeError(w, r, pointTypeCRUDErr(err))
		return
	}
	writeJSON(w, http.StatusCreated, pt)
}

func (s *Server) listPointTypes(w http.ResponseWriter, r *http.Request) {
	types, err := s.store.ListPointTypes(r.Context(), tenantFrom(r.Context()).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": types})
}

func (s *Server) getPointType(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "point_type_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	pt, err := s.store.GetPointType(r.Context(), tenantFrom(r.Context()).ID, id)
	if err != nil {
		s.writeError(w, r, pointTypeCRUDErr(err))
		return
	}
	writeJSON(w, http.StatusOK, pt)
}

func (s *Server) updatePointType(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "point_type_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req pointTypeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	pt, err := s.store.UpdatePointType(r.Context(), tenantFrom(r.Context()).ID, id, func(pt *store.PointType) error {
		req.apply(pt)
		return validatePointType(*pt)
	})
	if err != nil {
		s.writeError(w, r, pointTypeCRUDErr(err))
		return
	}
	writeJSON(w, http.StatusOK, pt)
}

func (s *Server) deletePointType(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "point_type_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.store.DeletePointType(r.Context(), tenantFrom(r.Context()).ID, id); err != nil {
		s.writeError(w, r, pointTypeCRUDErr(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func pointTypeCRUDErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apperr.NotFound("point_type_not_found", "point type not found")
	case errors.Is(err, store.ErrConflict):
		return apperr.Conflict("point_type_slug_taken", "a point type with that slug already exists")
	case errors.Is(err, store.ErrPrimaryPointType):
		return apperr.Conflict("primary_point_type",
			"the primary point type can't be removed or demoted; make another type primary first")
	case errors.Is(err, store.ErrPointTypeInUse):
		return apperr.Conflict("point_type_in_use",
			"the point type is referenced by balances, rules, levels or ledger history")
	case errors.Is(err, store.ErrPointTypeNotSpendable):
		return apperr.Conflict("point_type_debited",
			"a rule debits this point type; remove those debits before making it non-spendable")
	}
	return err
}
