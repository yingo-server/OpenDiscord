package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// client is opchat's thin HTTP transport. opchat cannot import adminctl
// (both are package main), so this mirrors adminctl's client.go semantics:
// same credential file format, same headers, same error decoding.
type client struct {
	base       string
	token      string
	csrf       string
	csrfCookie string
	idem       string
	hc         *http.Client
}

type credentials struct {
	Server    string `json:"server"`
	Token     string `json:"token"`
	CSRF      string `json:"csrf_token"`
	Username  string `json:"username,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// credPath matches adminctl's client.go credPath exactly so both tools share
// ~/.config/ufot/adminctl.json.
func credPath() string {
	if p := os.Getenv("UFOT_ADMIN_CREDENTIALS"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "ufot", "adminctl.json")
}

func loadCredentials() (credentials, error) {
	var c credentials
	b, err := os.ReadFile(credPath())
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func saveCredentials(c credentials) error {
	path := credPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// removeCredentials deletes the shared credential file (logout equivalence).
func removeCredentials() error {
	err := os.Remove(credPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func newClient(base, token, csrf string) *client {
	return &client{
		base:       strings.TrimRight(base, "/"),
		token:      token,
		csrf:       csrf,
		csrfCookie: "oc_admin_csrf", // internal/admin/server.go default
		idem:       newUUID(),
		hc:         &http.Client{Timeout: 15 * time.Second},
	}
}

type statusError struct {
	Status    int
	Reason    string
	RequestID string
	Body      []byte
}

func (e *statusError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Reason)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// do performs one request. write=true adds the Idempotency-Key header like
// adminctl does for mutating calls.
func (c *client) do(method, path string, body any, write bool) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
		req.AddCookie(&http.Cookie{Name: c.csrfCookie, Value: c.csrf})
	}
	if write {
		req.Header.Set("Idempotency-Key", c.idem)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		se := &statusError{Status: resp.StatusCode, Body: raw}
		var eb struct {
			Code      int    `json:"code"`
			Reason    string `json:"reason"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(raw, &eb) == nil {
			se.Reason = eb.Reason
			se.RequestID = eb.RequestID
			if se.Reason == "" {
				se.Reason = http.StatusText(resp.StatusCode)
			}
		}
		return nil, se
	}
	return raw, nil
}

func (c *client) get(path string) ([]byte, error) { return c.do("GET", path, nil, false) }
func (c *client) post(path string, b any) ([]byte, error) {
	return c.do("POST", path, b, true)
}
func (c *client) put(path string, b any) ([]byte, error) { return c.do("PUT", path, b, true) }

func queryPath(base string, params map[string]string) string {
	vals := url.Values{}
	for k, v := range params {
		if v != "" && v != "0" {
			vals.Set(k, v)
		}
	}
	if len(vals) == 0 {
		return base
	}
	return base + "?" + vals.Encode()
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = randRead(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randRead(b []byte) (int, error) {
	// crypto/rand without extra imports gymnastics: use math-free approach.
	f, err := os.Open("/dev/urandom")
	if err != nil {
		// degenerate fallback: time-based (still unique enough for idempotency keys)
		n, _ := time.Now().UnixNano(), 0
		for i := range b {
			b[i] = byte(n >> (uint(i%8) * 8))
			n = n*6364136223846793005 + 1442695040888963407
		}
		return len(b), nil
	}
	defer f.Close()
	return io.ReadFull(f, b)
}

// parseMetrics extracts simple metric values from Prometheus text.
func parseMetrics(raw string) map[string]float64 {
	out := map[string]float64{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.LastIndex(line, " ")
		if idx < 0 {
			continue
		}
		name, v := line[:idx], line[idx+1:]
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		out[name] = f
	}
	return out
}

// overview is the snapshot shown on the overview tab.
type overview struct {
	Online   float64
	Users    float64
	MsgToday float64
	Archive  string
	Raw      map[string]float64
}

func fetchOverview(c *client) (overview, error) {
	var ov overview
	ov.Archive = "-"
	mb, err := c.get("/api/v1/metrics")
	if err == nil {
		ov.Raw = parseMetrics(string(mb))
		ov.Online = metricAny(ov.Raw,
			"ufot_online_connections", "ufot_ws_connections",
			"ufot_active_connections", "ufot_online_users")
		ov.Users = metricAny(ov.Raw, "ufot_users_total", "ufot_user_total")
		ov.MsgToday = metricAny(ov.Raw, "ufot_messages_today_total",
			"ufot_messages_total_today", "ufot_message_send_total")
	}
	if ab, err := c.get("/api/v1/archive/status"); err == nil {
		ov.Archive = compactJSON(ab)
	}
	return ov, nil
}

func metricAny(m map[string]float64, names ...string) float64 {
	for _, n := range names {
		if v, ok := m[n]; ok {
			return v
		}
	}
	return 0
}

func compactJSON(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	s := buf.String()
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// --- typed helpers over the admin REST surface (paths mirror adminctl) ---

type userRow struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Status   int    `json:"status"`
	Phone    string `json:"phone"`
	Email    string `json:"email"`
}

func listUsers(c *client, q string, limit int) ([]userRow, error) {
	raw, err := c.get(queryPath("/api/v1/users", map[string]string{
		"q": q, "limit": strconv.Itoa(limit), "status": "-1",
	}))
	if err != nil {
		return nil, err
	}
	var r struct {
		Items []userRow `json:"items"`
		Users []userRow `json:"users"`
	}
	_ = json.Unmarshal(raw, &r)
	if len(r.Items) > 0 {
		return r.Items, nil
	}
	return r.Users, nil
}

func getUser(c *client, uid int64) (map[string]any, error) {
	raw, err := c.get(fmt.Sprintf("/api/v1/users/%d", uid))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func banUser(c *client, uid int64, level, days int, reason string) error {
	_, err := c.post(fmt.Sprintf("/api/v1/users/%d/ban", uid), map[string]any{
		"level": level, "days": days, "until": int64(0), "reason": reason,
	})
	return err
}

func unbanUser(c *client, uid int64, reason string) error {
	_, err := c.post(fmt.Sprintf("/api/v1/users/%d/unban", uid), map[string]any{"reason": reason})
	return err
}

func kickUser(c *client, uid int64, reason string) error {
	_, err := c.post(fmt.Sprintf("/api/v1/users/%d/kick", uid), map[string]any{"reason": reason})
	return err
}

type reportRow struct {
	ID     int64  `json:"id"`
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

func listReports(c *client, status, limit int) ([]reportRow, error) {
	raw, err := c.get(queryPath("/api/v1/reports", map[string]string{
		"status": strconv.Itoa(status), "limit": strconv.Itoa(limit),
	}))
	if err != nil {
		return nil, err
	}
	var r struct {
		Items []reportRow `json:"items"`
	}
	_ = json.Unmarshal(raw, &r)
	return r.Items, nil
}

func handleReport(c *client, id int64, action, note string) error {
	_, err := c.post(fmt.Sprintf("/api/v1/reports/%d/handle", id),
		map[string]any{"action": action, "note": note})
	return err
}

func broadcast(c *client, text string, dry bool) ([]byte, error) {
	body := map[string]any{"text": text, "conv_ids": []int64{}, "expires_at": 0, "dry_run": dry}
	path := "/api/v1/broadcast"
	if dry {
		path = queryPath(path, map[string]string{"dry_run": "true"})
	}
	return c.post(path, body)
}

type flagRow struct {
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Version int             `json:"version"`
}

func listFlags(c *client) ([]flagRow, error) {
	raw, err := c.get("/api/v1/flags")
	if err != nil {
		return nil, err
	}
	var r struct {
		Items []flagRow `json:"items"`
	}
	_ = json.Unmarshal(raw, &r)
	sort.Slice(r.Items, func(i, j int) bool { return r.Items[i].Key < r.Items[j].Key })
	return r.Items, nil
}

func setFlag(c *client, key, value string) error {
	_, err := c.put("/api/v1/flags", map[string]any{"key": key, "value": value})
	return err
}

func archiveStatus(c *client) ([]byte, error) { return c.get("/api/v1/archive/status") }

func archiveRun(c *client, dry bool) ([]byte, error) {
	path := "/api/v1/archive/run"
	if dry {
		path = queryPath(path, map[string]string{"dry_run": "true"})
	}
	return c.post(path, map[string]any{"dry_run": dry})
}

type auditRow struct {
	ID  int64           `json:"id"`
	Act string          `json:"action"`
	At  string          `json:"created_at"`
	Raw json.RawMessage `json:"-"`
}

func listAudit(c *client, sinceID int64, limit int) ([]auditRow, error) {
	raw, err := c.get(queryPath("/api/v1/audit", map[string]string{
		"since_id": strconv.FormatInt(sinceID, 10), "limit": strconv.Itoa(limit),
	}))
	if err != nil {
		return nil, err
	}
	var r struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	rows := make([]auditRow, 0, len(r.Items))
	for _, it := range r.Items {
		var m struct {
			ID        int64  `json:"id"`
			Action    string `json:"action"`
			CreatedAt string `json:"created_at"`
		}
		_ = json.Unmarshal(it, &m)
		rows = append(rows, auditRow{ID: m.ID, Act: m.Action, At: m.CreatedAt, Raw: it})
	}
	return rows, nil
}

func whoami(c *client) error {
	_, err := c.get("/api/v1/whoami")
	return err
}

// login performs the /api/v1/login exchange and saves shared credentials.
func login(base, user, password, totp string) error {
	c := newClient(base, "", "")
	raw, err := c.post("/api/v1/login", map[string]string{
		"username": user, "password": password, "totp": totp,
	})
	if err != nil {
		return err
	}
	var lr struct {
		Token     string `json:"token"`
		CSRF      string `json:"csrf_token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &lr); err != nil {
		return err
	}
	if lr.Token == "" {
		return fmt.Errorf("login response did not contain a token")
	}
	return saveCredentials(credentials{
		Server: base, Token: lr.Token, CSRF: lr.CSRF,
		Username: user, ExpiresAt: lr.ExpiresAt,
	})
}
