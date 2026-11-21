package handler

import (
	"net/http"

	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

func (a *API) handleBroadcast(w http.ResponseWriter, r *http.Request) {
	var req service.BroadcastInput
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	if queryBool(r, "dry_run") {
		req.DryRun = true
	}
	actor := middleware.IdentityFrom(r.Context())
	if req.DryRun {
		preview, err := a.deps.Broadcast.Preview(r.Context(), req)
		if err != nil {
			a.writeServiceError(w, r, err)
			return
		}
		middleware.WriteJSON(w, r, http.StatusOK, preview)
		return
	}
	res, err := a.deps.Broadcast.Send(r.Context(), actor, req)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, res)
}

func (a *API) handleAudit(w http.ResponseWriter, r *http.Request) {
	sinceID, err := queryInt64(r, "since_id", 0)
	if err != nil || sinceID < 0 {
		a.writeServiceError(w, r, service.ErrInvalid)
		return
	}
	limit, err := queryInt(r, "limit", 500)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	page, err := a.deps.Audit.List(r.Context(), middleware.IdentityFrom(r.Context()), sinceID, limit)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, page)
}
