package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
	"github.com/yingo-server/UFOT/server/internal/admin/service"
)

const maxBodyBytes = 1 << 20 // 1 MiB, security-spec E-7

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: empty body", service.ErrInvalid)
		}
		return fmt.Errorf("%w: invalid json: %v", service.ErrInvalid, err)
	}
	return nil
}

// decodeOptionalJSON accepts an empty body as a zero value (for actions with
// only optional fields such as unban reason).
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("%w: invalid json: %v", service.ErrInvalid, err)
	}
	return nil
}

func parseID(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: invalid %s", service.ErrInvalid, name)
	}
	return id, nil
}

func queryInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid %s", service.ErrInvalid, name)
	}
	return v, nil
}

func queryInt64(r *http.Request, name string, def int64) (int64, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid %s", service.ErrInvalid, name)
	}
	return v, nil
}

func queryBool(r *http.Request, name string) bool {
	switch r.URL.Query().Get(name) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

// writeServiceError maps service sentinels onto the unified error body.
func (a *API) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, service.ErrNotFound):
		middleware.WriteError(w, r, http.StatusNotFound, "not_found")
	case errors.Is(err, service.ErrConflict):
		middleware.WriteError(w, r, http.StatusConflict, "conflict")
	case errors.Is(err, service.ErrInvalid):
		middleware.WriteError(w, r, http.StatusBadRequest, "invalid_argument")
	case errors.Is(err, service.ErrUnauthorized):
		middleware.WriteError(w, r, http.StatusUnauthorized, "unauthenticated")
	case errors.Is(err, service.ErrForbidden):
		middleware.WriteError(w, r, http.StatusForbidden, "permission_denied")
	case errors.Is(err, service.ErrUnavailable):
		w.Header().Set("Retry-After", "5")
		middleware.WriteError(w, r, http.StatusServiceUnavailable, "bridge_unavailable")
	case errors.Is(err, service.ErrNotConfigured):
		middleware.WriteError(w, r, http.StatusServiceUnavailable, "not_configured")
	case errors.Is(err, context.DeadlineExceeded):
		middleware.WriteError(w, r, http.StatusGatewayTimeout, "timeout")
	case errors.Is(err, context.Canceled):
		middleware.WriteError(w, r, http.StatusRequestTimeout, "request_canceled")
	default:
		a.deps.Logger.Error("admin handler error",
			"request_id", middleware.RequestID(r.Context()),
			"path", r.URL.Path,
			"err", err.Error())
		middleware.WriteError(w, r, http.StatusInternalServerError, "internal_error")
	}
}
