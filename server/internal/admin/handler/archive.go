package handler

import (
	"net/http"

	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
)

func (a *API) handleArchiveStatus(w http.ResponseWriter, r *http.Request) {
	st, err := a.deps.Archive.Status(r.Context())
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, st)
}

type archiveRunRequest struct {
	DryRun bool `json:"dry_run"`
}

func (a *API) handleArchiveRun(w http.ResponseWriter, r *http.Request) {
	var req archiveRunRequest
	if err := decodeOptionalJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	dry := req.DryRun || queryBool(r, "dry_run")
	run, err := a.deps.Archive.Run(r.Context(), middleware.IdentityFrom(r.Context()), dry)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, run)
}

func (a *API) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if id := middleware.RequestID(r.Context()); id != "" {
		w.Header().Set("X-Request-ID", id)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(a.deps.Metrics.Render()))
}
