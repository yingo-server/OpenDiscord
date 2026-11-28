package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
	"github.com/yingo-server/UFOT/server/internal/admin/identity"
)

func actorName(a *identity.Admin) string {
	if a == nil {
		return "system"
	}
	if a.Username == "" {
		return "admin:" + itoa(a.AdminID)
	}
	return a.Username
}

func normalizeLimit(limit, def, max int) int {
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

func normalizeOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}

// injectJSON marshals v and calls the dispatcher, translating transport
// failures into service sentinels.
func injectJSON(ctx context.Context, d Dispatcher, kind uint32, target bridgeproxy.Target, v any) (bridgeproxy.Result, error) {
	if d == nil {
		return bridgeproxy.Result{}, ErrNotConfigured
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return bridgeproxy.Result{}, fmt.Errorf("%w: marshal payload: %v", ErrInvalid, err)
	}
	res, err := d.InjectFrame(ctx, kind, target, payload)
	if err != nil {
		if errors.Is(err, bridgeproxy.ErrCircuitOpen) {
			return bridgeproxy.Result{}, fmt.Errorf("%w: bridge circuit open", ErrUnavailable)
		}
		if errors.Is(err, bridgeproxy.ErrUnavailable) {
			return bridgeproxy.Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return bridgeproxy.Result{}, err
	}
	return res, nil
}

// MaskPhone masks a phone number for display/logs (138****1234).
func MaskPhone(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if len(p) < 7 {
		return "****"
	}
	return p[:3] + "****" + p[len(p)-4:]
}

// MaskEmail masks an email for display/logs (a***@domain).
func MaskEmail(e string) string {
	e = strings.TrimSpace(e)
	if e == "" {
		return ""
	}
	at := strings.LastIndexByte(e, '@')
	if at <= 0 || at == len(e)-1 {
		return "***"
	}
	return e[:1] + "***@" + e[at+1:]
}

func maskUser(u UserRecord) UserRecord {
	u.Phone = MaskPhone(u.Phone)
	u.Email = MaskEmail(u.Email)
	return u
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
