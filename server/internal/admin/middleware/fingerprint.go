package middleware

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
)

var fingerprintKey [32]byte

func init() {
	if _, err := rand.Read(fingerprintKey[:]); err != nil {
		// crypto/rand failure is unrecoverable for security code, but a fixed
		// salt only degrades cross-process unlinkability, not lockout logic.
		copy(fingerprintKey[:], []byte("ufot-admin-fingerprint-key"))
	}
}

// ClientIP extracts the caller address. X-Forwarded-For is honoured only when
// the deployment explicitly enables TrustProxy (Caddy is the expected proxy).
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if first != "" {
				return first
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Fingerprint derives a stable, non-reversible source identity from
// IP + normalized User-Agent. It is used as the second lockout dimension
// (A-1: 账号+来源指纹双计数). Raw IPs are never stored in limiter maps.
func Fingerprint(r *http.Request, trustProxy bool) string {
	ip := ClientIP(r, trustProxy)
	ua := normalizeUA(r.UserAgent())
	mac := hmac.New(sha256.New, fingerprintKey[:])
	mac.Write([]byte(ip))
	mac.Write([]byte{0})
	mac.Write([]byte(ua))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

func normalizeUA(ua string) string {
	ua = strings.ToLower(ua)
	ua = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, ua)
	ua = strings.Join(strings.Fields(ua), " ")
	if len(ua) > 160 {
		ua = ua[:160]
	}
	return ua
}
