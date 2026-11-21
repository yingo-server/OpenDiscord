package handler

import (
	"net/http"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

func (a *API) handleUserList(w http.ResponseWriter, r *http.Request) {
	status, err := queryInt(r, "status", -1)
	if err != nil || status < -1 || status > 3 {
		a.writeServiceError(w, r, service.ErrInvalid)
		return
	}
	limit, err := queryInt(r, "limit", 50)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	page, err := a.deps.Users.List(r.Context(), middleware.IdentityFrom(r.Context()), service.UserFilter{
		Query: q, Status: status, Limit: limit, Offset: offset,
	})
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, page)
}

func (a *API) handleUserShow(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	u, err := a.deps.Users.Show(r.Context(), middleware.IdentityFrom(r.Context()), id)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, u)
}

type banRequest struct {
	Level  int    `json:"level"`
	Reason string `json:"reason"`
	Days   int    `json:"days"`
	Until  int64  `json:"until"`
}

func (a *API) handleUserBan(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	var req banRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	if req.Level == 0 {
		req.Level = 1
	}
	res, err := a.deps.Users.Ban(r.Context(), middleware.IdentityFrom(r.Context()), id, service.BanInput{
		Level: req.Level, Reason: req.Reason, Days: req.Days, Until: req.Until,
	})
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, res)
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func (a *API) handleUserUnban(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	var req reasonRequest
	if err := decodeOptionalJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	res, err := a.deps.Users.Unban(r.Context(), middleware.IdentityFrom(r.Context()), id, req.Reason)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, res)
}

func (a *API) handleUserKick(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	var req reasonRequest
	if err := decodeOptionalJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	res, err := a.deps.Users.Kick(r.Context(), middleware.IdentityFrom(r.Context()), id, req.Reason)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, res)
}
