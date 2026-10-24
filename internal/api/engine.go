package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/samiul-kabir-26/points-calculator/internal/apperr"
	"github.com/samiul-kabir-26/points-calculator/internal/engine"
	"github.com/samiul-kabir-26/points-calculator/internal/store"
)

type resultResponse struct {
	store.LedgerEntry
	LevelChange *store.LevelChange `json:"level_change,omitempty"`
	Streak      *streakBody        `json:"streak,omitempty"`
	Replayed    bool               `json:"replayed"`
}

type streakBody struct {
	// Days is how many consecutive local days the entity has claimed, including today.
	Days int `json:"days"`
}

// writeResult returns 201 for a newly applied entry and 200 for an idempotent replay.
func writeResult(w http.ResponseWriter, res engine.Result) {
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	out := resultResponse{LedgerEntry: res.Entry, LevelChange: res.LevelChange, Replayed: res.Replayed}
	if res.Streak != nil {
		out.Streak = &streakBody{Days: *res.Streak}
	}
	writeJSON(w, status, out)
}

// execute handles POST /engine/execute. The entity may be given as "entity_id" or
// under the tenant's architecture key (e.g. "user_id"); numeric IDs are accepted.
func (s *Server) execute(w http.ResponseWriter, r *http.Request) {
	tenant := tenantFrom(r.Context())
	var body map[string]json.RawMessage
	if err := decodeJSON(w, r, &body); err != nil {
		s.writeError(w, r, err)
		return
	}

	allowed := map[string]bool{
		"entity_id": true, "tier": true, "rule": true, "spend": true, "amount": true, "idempotency_key": true,
	}
	archKey := ""
	if tenant.ArchitectureKey != nil {
		archKey = *tenant.ArchitectureKey
		allowed[archKey] = true
	}
	var unknown []string
	for k := range body {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		s.writeError(w, r, apperr.BadRequest("unknown field(s): %s", strings.Join(unknown, ", ")))
		return
	}

	var f fieldErrors
	str := func(name string) string {
		v, err := scalarString(body[name])
		if err != nil {
			f.add(name + " must be a string")
		}
		return v
	}
	req := engine.ExecuteRequest{
		EntityID:       str("entity_id"),
		Tier:           str("tier"),
		Rule:           str("rule"),
		IdempotencyKey: str("idempotency_key"),
	}
	spend, err := scalarInt(body["spend"])
	if err != nil {
		f.add("spend must be an integer")
	}
	req.Spend = spend
	if raw := bytes.TrimSpace(body["amount"]); len(raw) > 0 && string(raw) != "null" {
		amount, err := scalarInt(raw)
		if err != nil || amount < 1 || amount > maxPoints {
			f.add("amount must be an integer between 1 and 10^15")
		}
		req.Amount = &amount
	}
	entityField := "entity_id"
	if archKey != "" && archKey != "entity_id" {
		if alias := str(archKey); alias != "" {
			if req.EntityID != "" && req.EntityID != alias {
				f.add("entity_id and " + archKey + " disagree")
			}
			req.EntityID = alias
		}
		if req.EntityID == "" {
			entityField = "entity_id (or " + archKey + ")"
		}
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = r.Header.Get("Idempotency-Key")
	}
	validateEntityID(&f, entityField, req.EntityID)
	validateSpend(&f, req.Spend)
	if req.Rule == "" {
		f.add("rule is required")
	}
	validateIdempotencyKey(&f, req.IdempotencyKey)
	if err := f.err(); err != nil {
		s.writeError(w, r, err)
		return
	}

	res, err := s.engine.Execute(r.Context(), tenant, req)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeResult(w, res)
}

// scalarInt reads a JSON integer. Absent or null yields 0.
func scalarInt(raw json.RawMessage) (int64, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var n int64
	err := json.Unmarshal(raw, &n)
	return n, err
}

// scalarString reads a JSON string or number as a string. Absent or null yields "".
func scalarString(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		err := json.Unmarshal(raw, &s)
		return s, err
	}
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return "", err
	}
	return n.String(), nil
}
