package handler

import (
	"net/http"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

func (a *API) handleReportList(w http.ResponseWriter, r *http.Request) {
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
	page, err := a.deps.Reports.List(r.Context(), middleware.IdentityFrom(r.Context()), service.ReportFilter{
		Status: status, Limit: limit, Offset: offset,
	})
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, page)
}

func (a *API) handleReportShow(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	rep, err := a.deps.Reports.Show(r.Context(), middleware.IdentityFrom(r.Context()), id)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, rep)
}

type handleReportRequest struct {
	Action string `json:"action"`
	Note   string `json:"note"`
}

func (a *API) handleReportHandle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	var req handleReportRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	rep, err := a.deps.Reports.Handle(r.Context(), middleware.IdentityFrom(r.Context()), id, req.Action, req.Note)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, rep)
}

func (a *API) handleMessageSearch(w http.ResponseWriter, r *http.Request) {
	convID, err := queryInt64(r, "conv_id", 0)
	if err != nil || convID < 0 {
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
	page, err := a.deps.Messages.Search(r.Context(), middleware.IdentityFrom(r.Context()), service.MessageQuery{
		Query: strings.TrimSpace(r.URL.Query().Get("q")), ConvID: convID, Limit: limit, Offset: offset,
	})
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, page)
}

func (a *API) handleMessageRecall(w http.ResponseWriter, r *http.Request) {
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
	res, err := a.deps.Messages.Recall(r.Context(), middleware.IdentityFrom(r.Context()), id, req.Reason)
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, res)
}
