package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/monirujjaman-dev/docklet/internal/audit"
	"github.com/monirujjaman-dev/docklet/internal/store/db"
)

type auditDTO struct {
	ID      int64           `json:"id"`
	Ts      int64           `json:"ts"`
	Actor   string          `json:"actor"`
	Action  string          `json:"action"`
	Target  string          `json:"target"`
	IP      string          `json:"ip"`
	Details json.RawMessage `json:"details"`
	Hash    string          `json:"hash"`
}

// GET /api/audit?before=<id>&limit=<n>
func (s *Server) handleAuditList(w http.ResponseWriter, r *http.Request) error {
	before := int64(math.MaxInt64)
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			return errf(http.StatusBadRequest, "invalid 'before'")
		}
		before = n
	}
	limit := int64(50)
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 || n > 200 {
			return errf(http.StatusBadRequest, "'limit' must be 1-200")
		}
		limit = n
	}
	rows, err := s.st.Q.ListAuditEntries(r.Context(), db.ListAuditEntriesParams{BeforeID: before, Limit: limit})
	if err != nil {
		return err
	}
	out := make([]auditDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, auditDTO{
			ID: row.ID, Ts: row.Ts, Actor: row.Actor, Action: row.Action, Target: row.Target,
			IP: row.Ip, Details: json.RawMessage(row.Details), Hash: row.Hash,
		})
	}
	resp := map[string]any{"entries": out}
	if int64(len(rows)) == limit && len(rows) > 0 {
		resp["nextBefore"] = rows[len(rows)-1].ID
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// GET /api/audit/verify
func (s *Server) handleAuditVerify(w http.ResponseWriter, r *http.Request) error {
	res, err := audit.Verify(r.Context(), s.st.Q)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}
