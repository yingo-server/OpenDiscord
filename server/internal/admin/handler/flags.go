package handler

import (
	"net/http"
	"sort"

	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

func (a *API) handleFlagList(w http.ResponseWriter, r *http.Request) {
	flags, err := a.deps.Flags.List(r.Context())
	if err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	middleware.WriteJSON(w, r, http.StatusOK, map[string]any{"items": flags})
}

type setFlagRequest struct {
	Key   string            `json:"key"`
	Value string            `json:"value"`
	Flags map[string]string `json:"flags"`
}

func (a *API) handleFlagSet(w http.ResponseWriter, r *http.Request) {
	var req setFlagRequest
	if err := decodeJSON(w, r, &req); err != nil {
		a.writeServiceError(w, r, err)
		return
	}
	actor := middleware.IdentityFrom(r.Context())
	var out []service.Flag
	if len(req.Flags) > 0 {
		keys := make([]string, 0, len(req.Flags))
		for k := range req.Flags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f, err := a.deps.Flags.Set(r.Context(), actor, k, req.Flags[k])
			if err != nil {
				a.writeServiceError(w, r, err)
				return
			}
			out = append(out, *f)
		}
	} else {
		f, err := a.deps.Flags.Set(r.Context(), actor, req.Key, req.Value)
		if err != nil {
			a.writeServiceError(w, r, err)
			return
		}
		out = append(out, *f)
	}
	middleware.WriteJSON(w, r, http.StatusOK, map[string]any{"items": out})
}
