package pushsvc

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
)

// StatusError carries the push service HTTP status so the service can drop
// dead subscriptions (404/410) without guessing.
type StatusError struct {
	Status int
	Err    error
}

func (e *StatusError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("push service status %d", e.Status)
	}
	return fmt.Sprintf("push service status %d: %s", e.Status, e.Err.Error())
}

func (e *StatusError) Unwrap() error { return e.Err }

// NoopSender drops every payload. It is the fail-closed default: without
// VAPID configuration the service still accepts subscriptions, but nothing is
// ever delivered (and it says so in the log, once, at construction).
type NoopSender struct {
	lg *log.Logger
}

// NewNoopSender builds the do-nothing sender.
func NewNoopSender(lg *log.Logger) *NoopSender {
	if lg == nil {
		lg = log.For("pushsvc")
	}
	return &NoopSender{lg: lg}
}

// Send discards the payload.
func (s *NoopSender) Send(sub Subscription, payload []byte) error {
	return nil
}

// WebPushSender is the real Web Push sender: VAPID (RFC 8292) authentication
// + aes128gcm payload encryption (RFC 8188 / RFC 8291).
type WebPushSender struct {
	priv      *ecdsa.PrivateKey
	ecdhPriv  *ecdh.PrivateKey
	pubBytes  []byte // uncompressed P-256 public key (65 bytes)
	subject   string // mailto:... (VAPID sub claim)
	lg        *log.Logger
	client    *http.Client
	now       func() time.Time
	expiresIn int64
}

// WebPushConfig configures the sender.
type WebPushConfig struct {
	// PrivateKey is the VAPID private key: PEM (PKCS#8 / SEC1), base64(PEM)
	// or base64 of the raw 32-byte P-256 scalar.
	PrivateKey []byte
	// Subject is the VAPID `sub` claim, normally mailto:admin@example.com.
	Subject string
	Logger  *log.Logger
	// HTTPClient overrides the default client (tests).
	HTTPClient *http.Client
	Now        func() time.Time
}

// NewWebPushSender builds the sender from an explicit key. It fails loudly:
// a wrong key is a configuration error, not a runtime degradation.
func NewWebPushSender(cfg WebPushConfig) (*WebPushSender, error) {
	if cfg.Logger == nil {
		cfg.Logger = log.For("pushsvc")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	subject := strings.TrimSpace(cfg.Subject)
	if subject == "" {
		return nil, errors.New("pushsvc: vapid subject is required")
	}
	if !strings.HasPrefix(subject, "mailto:") && !strings.HasPrefix(subject, "https://") {
		return nil, errors.New("pushsvc: vapid subject must be mailto: or https://")
	}
	key, err := parseVAPIDKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	pub := key.PublicKey
	pubBytes := elliptic.Marshal(elliptic.P256(), pub.X, pub.Y)
	ecdhKey, err := ecdh.P256().NewPrivateKey(padScalar(key.D.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("pushsvc: ecdh key: %w", err)
	}
	return &WebPushSender{
		priv:      key,
		ecdhPriv:  ecdhKey,
		pubBytes:  pubBytes,
		subject:   subject,
		lg:        cfg.Logger,
		client:    cfg.HTTPClient,
		now:       cfg.Now,
		expiresIn: 12 * 3600,
	}, nil
}

// SenderFromEnv builds the sender from the environment:
//
//	VAPID_PRIVATE_KEY  PEM (or base64 of PEM, or base64 of the raw scalar)
//	VAPID_SUBJECT      mailto:admin@example.com
//
// Missing or invalid configuration degrades to the no-op sender (fail-closed):
// subscriptions are still registered, nothing is delivered, and the operator
// sees a WARN.
func SenderFromEnv(lg *log.Logger) Sender {
	if lg == nil {
		lg = log.For("pushsvc")
	}
	key := strings.TrimSpace(os.Getenv("VAPID_PRIVATE_KEY"))
	subject := strings.TrimSpace(os.Getenv("VAPID_SUBJECT"))
	if key == "" || subject == "" {
		lg.Warn("push disabled: VAPID_PRIVATE_KEY/VAPID_SUBJECT not configured; subscriptions are registered but never delivered")
		return NewNoopSender(lg)
	}
	sender, err := NewWebPushSender(WebPushConfig{
		PrivateKey: []byte(key),
		Subject:    subject,
		Logger:     lg,
	})
	if err != nil {
		lg.Warn("push disabled: invalid VAPID configuration; subscriptions are registered but never delivered", "err", err.Error())
		return NewNoopSender(lg)
	}
	lg.Info("push sender ready (VAPID configured)")
	return sender
}

// Send delivers one payload to one subscription.
func (s *WebPushSender) Send(sub Subscription, payload []byte) error {
	uaPub, err := decodeB64URL(sub.P256DH)
	if err != nil || len(uaPub) != 65 {
		return errors.New("pushsvc: bad p256dh")
	}
	authSecret, err := decodeB64URL(sub.Auth)
	if err != nil || len(authSecret) != 16 {
		return errors.New("pushsvc: bad auth secret")
	}
	body, err := s.encryptPayload(uaPub, authSecret, payload)
	if err != nil {
		return err
	}
	jwt, err := s.vapidJWT(sub.Endpoint)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf("vapid t=%s, k=%s", jwt, base64.RawURLEncoding.EncodeToString(s.pubBytes)))
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Crypto-Key", "p256ecdsa="+base64.RawURLEncoding.EncodeToString(s.pubBytes))
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", "normal")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &StatusError{Status: resp.StatusCode, Err: errors.New("push service rejected the request")}
}

// vapidJWT builds the ES256 JWT (RFC 8292 §2): aud = endpoint origin,
// exp = now + 12h, sub = contact.
func (s *WebPushSender) vapidJWT(endpoint string) (string, error) {
	origin, err := endpointOrigin(endpoint)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	// JWT 的 payload 段必须 base64url 编码（不能直接放裸 JSON）——裸 JSON 里
	// 的域名/邮箱含 "."，会让三段 JWT 被误拆，且不符合 RFC 7519。
	claimsJSON := fmt.Sprintf(`{"aud":%q,"exp":%d,"sub":%q}`, origin, s.now().Unix()+s.expiresIn, s.subject)
	claims := base64.RawURLEncoding.EncodeToString([]byte(claimsJSON))
	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	r, sig, err := ecdsa.Sign(rand.Reader, s.priv, digest[:])
	if err != nil {
		return "", err
	}
	sigBytes := make([]byte, 64)
	r.FillBytes(sigBytes[:32])
	sig.FillBytes(sigBytes[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sigBytes), nil
}

// encryptPayload builds the aes128gcm body (RFC 8188 §2 + RFC 8291 §3.3):
// salt || rs || idlen || keyid || ciphertext.
func (s *WebPushSender) encryptPayload(uaPub, authSecret, payload []byte) ([]byte, error) {
	remote, err := ecdh.P256().NewPublicKey(uaPub)
	if err != nil {
		return nil, fmt.Errorf("pushsvc: bad subscriber key: %w", err)
	}
	shared, err := s.ecdhPriv.ECDH(remote)
	if err != nil {
		return nil, fmt.Errorf("pushsvc: ecdh: %w", err)
	}
	cek := make([]byte, 16)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, authSecret, []byte("Content-Encoding: aes128gcm\x00")), cek); err != nil {
		return nil, fmt.Errorf("pushsvc: hkdf cek: %w", err)
	}
	// PRK = HMAC-SHA-256(auth_secret, ecdh_secret) — the RFC 8291 value.
	prk := hmacSum(authSecret, shared)
	nonce := expand(prk, []byte("Content-Encoding: nonce\x00"), 12)

	block, err := aesGCM(cek, nonce, payload)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("pushsvc: salt: %w", err)
	}
	out := make([]byte, 0, len(salt)+4+1+len(s.pubBytes)+len(block))
	out = append(out, salt...)
	rs := uint32(len(block))
	out = append(out, byte(rs>>24), byte(rs>>16), byte(rs>>8), byte(rs))
	out = append(out, byte(len(s.pubBytes)))
	out = append(out, s.pubBytes...)
	out = append(out, block...)
	return out, nil
}

// aesGCM seals the payload with AES-128-GCM (RFC 8188).
func aesGCM(cek, nonce, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, fmt.Errorf("pushsvc: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("pushsvc: gcm: %w", err)
	}
	return gcm.Seal(nil, nonce, plaintext, nil), nil
}

// parseVAPIDKey accepts PEM (PKCS#8 or SEC1), base64(PEM) or base64(raw
// 32-byte P-256 scalar).
func parseVAPIDKey(raw []byte) (*ecdsa.PrivateKey, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, errors.New("pushsvc: vapid private key is empty")
	}
	if strings.Contains(trimmed, "-----BEGIN") {
		return parsePEM([]byte(trimmed))
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(trimmed)
		if err != nil {
			return nil, errors.New("pushsvc: vapid private key must be PEM or base64")
		}
	}
	if bytes.Contains(decoded, []byte("-----BEGIN")) {
		return parsePEM(decoded)
	}
	if len(decoded) != 32 {
		return nil, errors.New("pushsvc: vapid raw scalar must be 32 bytes")
	}
	d := new(big.Int)
	d.SetBytes(decoded)
	priv := &ecdsa.PrivateKey{D: d}
	priv.PublicKey.Curve = elliptic.P256()
	priv.PublicKey.X, priv.PublicKey.Y = elliptic.P256().ScalarBaseMult(decoded)
	return priv, nil
}

// parsePEM decodes a PEM private key block (PKCS#8 first, then SEC1).
func parsePEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("pushsvc: vapid private key PEM is malformed")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ec, ok := key.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("pushsvc: vapid private key must be ECDSA P-256")
		}
		return ec, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("pushsvc: vapid private key PEM is not a P-256 EC key")
}

// endpointOrigin returns the origin (scheme://host) of a push endpoint.
func endpointOrigin(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("pushsvc: invalid endpoint")
	}
	return u.Scheme + "://" + u.Host, nil
}

// hmacSum is HMAC-SHA-256(key, msg).
func hmacSum(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}

// expand is the single-block HKDF-Expand step (RFC 5869).
func expand(prk, info []byte, length int) []byte {
	h := hmac.New(sha256.New, prk)
	h.Write(info)
	h.Write([]byte{0x01})
	sum := h.Sum(nil)
	if length > len(sum) {
		length = len(sum)
	}
	return sum[:length]
}

// padScalar left-pads a scalar to 32 bytes (P-256 field size).
func padScalar(b []byte) []byte {
	out := make([]byte, 32)
	if len(b) >= 32 {
		copy(out, b[len(b)-32:])
		return out
	}
	copy(out[32-len(b):], b)
	return out
}
