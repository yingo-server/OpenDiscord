package mediasvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/data/store/sqlite"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

const testToken = "tok"

func authed(r *http.Request) *http.Request {
	r.AddCookie(&http.Cookie{Name: "access_token", Value: testToken})
	return r
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Dir:      t.TempDir(),
		Verifier: &fakeVerifier{},
		Media:    &fakeMediaStore{},
		Sys:      &fakeSysStore{},
		Logger:   log.For("mediasvc-test"),
	}
}

func uploadRequest(t *testing.T, name string, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("write file: %v", err)
	}
	w.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/media/upload", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

// uploadRequestMeta is uploadRequest plus extra ordinary form fields
// (thumb_data / waveform), mirroring a browser multipart upload.
func uploadRequestMeta(t *testing.T, name string, body []byte, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", name)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	w.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/media/upload", &buf)
	r.Header.Set("Content-Type", w.FormDataContentType())
	return r
}

func TestUploadRequiresAuth(t *testing.T) {
	s, err := New(testDeps(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, uploadRequest(t, "x.png", pngBytes(t)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestUploadDownloadPNG(t *testing.T) {
	d := testDeps(t)
	s, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "pic.png", pngBytes(t))))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		MediaID string `json:"media_id"`
		Size    int    `json:"size"`
		Mime    string `json:"mime"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !validMediaID(resp.MediaID) || len(resp.MediaID) != 32 {
		t.Fatalf("bad media_id %q (len %d)", resp.MediaID, len(resp.MediaID))
	}
	if resp.Mime != "image/png" {
		t.Fatalf("mime = %q", resp.Mime)
	}

	// Download round-trips the bytes.
	g := httptest.NewRequest(http.MethodGet, "/api/media/"+resp.MediaID, nil)
	gr := httptest.NewRecorder()
	s.Routes().ServeHTTP(gr, authed(g))
	if gr.Code != http.StatusOK {
		t.Fatalf("get status = %d", gr.Code)
	}
	if !bytes.Equal(gr.Body.Bytes(), pngBytes(t)) {
		t.Fatal("downloaded bytes differ")
	}
	if ct := gr.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}
	if gr.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing nosniff")
	}

	// Row landed in the store with status=1 and the uploader's uid.
	ms := d.Media.(*fakeMediaStore)
	row := ms.rows[resp.MediaID]
	if row == nil || row.Status != 1 || row.UID != fakeUID || row.MType != mtypeImage {
		t.Fatalf("bad row %+v", row)
	}
	// File exists under sha256-sharded path.
	if _, err := os.Stat(filepath.Join(d.Dir, row.SHA256[:2], row.SHA256[2:4], row.SHA256)); err != nil {
		t.Fatalf("file missing: %v", err)
	}
}

func TestUploadRejectsBadTypes(t *testing.T) {
	s, _ := New(testDeps(t))

	cases := []struct {
		name string
		body []byte
	}{
		{"evil.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"evil.exe", []byte("MZ\x90\x00whatever")},
		{"png.exe", pngBytes(t)}, // extension/content mismatch
		{"script.js", []byte("alert(1)")},
		{"noext", []byte("plain text without extension")},
	}
	for _, tc := range cases {
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, authed(uploadRequest(t, tc.name, tc.body)))
		if rr.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s: status = %d, want 415 (body=%s)", tc.name, rr.Code, rr.Body.String())
		}
	}
}

func TestUploadSizeCaps(t *testing.T) {
	s, _ := New(testDeps(t))

	// An audio file above 2MB is rejected (even with valid OGG magic).
	big := append([]byte("OggS"), bytes.Repeat([]byte{0}, maxAudioBytes+1)...)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "voice.ogg", big)))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("audio cap status = %d, want 413", rr.Code)
	}

	// A generic file above 20MB is rejected (whitelisted .txt, no magic).
	bigFile := bytes.Repeat([]byte{0x42}, maxFileBytes+1)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "big.txt", bigFile)))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("file cap status = %d, want 413", rr.Code)
	}
}

func TestDailyRateLimit(t *testing.T) {
	s, _ := New(testDeps(t))
	for i := 0; i < dailyUploadLimit; i++ {
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "a.png", pngBytes(t))))
		if rr.Code != http.StatusCreated {
			t.Fatalf("upload %d: status = %d body=%s", i, rr.Code, rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "a.png", pngBytes(t))))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-cap status = %d, want 429", rr.Code)
	}
}

func TestGetUnknownIDIs404(t *testing.T) {
	s, _ := New(testDeps(t))
	g := httptest.NewRequest(http.MethodGet, "/api/media/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", nil)
	gr := httptest.NewRecorder()
	s.Routes().ServeHTTP(gr, authed(g))
	if gr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", gr.Code)
	}
}

// -------------------------------------------------------------- test fakes

const fakeUID int64 = 777

// fakeVerifier authorizes only the fixed test token, as fakeUID.
type fakeVerifier struct{}

func (f *fakeVerifier) Verify(token string) (*authn.Identity, error) {
	if token != testToken {
		return nil, errors.New("bad token")
	}
	return &authn.Identity{UserID: fakeUID}, nil
}

type fakeMediaStore struct{ rows map[string]*store.Media }

func (f *fakeMediaStore) Insert(_ context.Context, m *store.Media) error {
	if f.rows == nil {
		f.rows = map[string]*store.Media{}
	}
	f.rows[m.MediaID] = m
	return nil
}

func (f *fakeMediaStore) Get(_ context.Context, id string) (*store.Media, error) {
	if m, ok := f.rows[id]; ok {
		return m, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeMediaStore) UpdateStatus(_ context.Context, id string, status int) error {
	f.rows[id].Status = status
	return nil
}

func (f *fakeMediaStore) IncRefCount(_ context.Context, id string, delta int64) error {
	f.rows[id].RefCount += delta
	return nil
}

func (f *fakeMediaStore) AddRef(ctx context.Context, id string) error {
	return f.IncRefCount(ctx, id, 1)
}

type fakeSysStore struct{ count int64 }

func (f *fakeSysStore) UpsertFlag(_ context.Context, _ *store.Flag) error { return nil }
func (f *fakeSysStore) AllFlags(_ context.Context) ([]store.Flag, error)  { return nil, nil }
func (f *fakeSysStore) UpsertPresence(_ context.Context, _ *store.Presence) error {
	return nil
}
func (f *fakeSysStore) GetPresence(_ context.Context, _ int64) (*store.Presence, error) {
	return nil, nil
}
func (f *fakeSysStore) GetPenalty(_ context.Context, _, _ string) (*store.Penalty, error) {
	return nil, nil
}
func (f *fakeSysStore) SetPenalty(_ context.Context, _ *store.Penalty) error { return nil }

func (f *fakeSysStore) RateLimitIncr(_ context.Context, _, _ string, _ int64) (*store.RateWindow, error) {
	f.count++
	return &store.RateWindow{Count: f.count, WindowLength: 86400}, nil
}

// -------------------------------------------------------------- ACL + whitelist tests

const uid2 int64 = 888

func authedTok(r *http.Request, token string) *http.Request {
	r.AddCookie(&http.Cookie{Name: "access_token", Value: token})
	return r
}

// mapVerifier maps tokens to uids so a test can play distinct users.
type mapVerifier struct{ tokens map[string]int64 }

func (m *mapVerifier) Verify(token string) (*authn.Identity, error) {
	uid, ok := m.tokens[token]
	if !ok {
		return nil, errors.New("bad token")
	}
	return &authn.Identity{UserID: uid}, nil
}

// newServiceDB builds a service backed by a real migrated sqlite hot DB so the
// media GET ACL (msg scan + membership) runs against real SQL.
func newServiceDB(t *testing.T) (*Service, *sqlite.DB) {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "hot.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	d := Deps{
		Dir:      t.TempDir(),
		Verifier: &mapVerifier{tokens: map[string]int64{testToken: fakeUID, "u2": uid2}},
		Media:    &fakeMediaStore{},
		Sys:      &fakeSysStore{},
		Conv:     db,
		QueryDB:  db.DB,
		Logger:   log.For("mediasvc-test"),
	}
	s, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, db
}

func uploadPNGID(t *testing.T, s *Service) string {
	t.Helper()
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "p.png", pngBytes(t))))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.MediaID
}

func getMedia(s *Service, id, token string) *httptest.ResponseRecorder {
	g := httptest.NewRequest(http.MethodGet, "/api/media/"+id, nil)
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authedTok(g, token))
	return rr
}

// GET ACL: owner passes; an unrelated user gets 404 (not 403); a non-owner
// member of a conversation that references the media_id gets 200.
func TestGetMediaACL(t *testing.T) {
	s, db := newServiceDB(t)
	ctx := context.Background()
	mediaID := uploadPNGID(t, s)

	// Owner (uploader) → 200.
	if rr := getMedia(s, mediaID, testToken); rr.Code != http.StatusOK {
		t.Fatalf("owner GET status = %d body=%s", rr.Code, rr.Body.String())
	}

	// Unrelated user, nobody references the media → 404 (anti-enumeration).
	if rr := getMedia(s, mediaID, "u2"); rr.Code != http.StatusNotFound {
		t.Fatalf("unrelated GET status = %d, want 404", rr.Code)
	}

	// Seed a conversation the second user belongs to, with a message that
	// references the media_id — so its content LIKE matches.
	cid, err := db.CreateConv(ctx, &store.ConvMeta{CType: 2, Title: "t", CreatedBy: fakeUID, CreatedAt: 1}, fakeUID)
	if err != nil {
		t.Fatalf("CreateConv: %v", err)
	}
	if err := db.AddMember(ctx, &store.Member{Cid: cid, UID: uid2, Role: 1, JoinedAt: 1}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	content := []byte(`{"text":"hi","media":[{"mediaId":"` + mediaID + `"}]}`)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO msg_messages_hot (msg_id, cid, seq, sender_id, client_msg_id, content, sent_at) VALUES (?,?,?,?,?,?,?)`,
		int64(1), cid, int64(1), fakeUID, "c1", content, int64(1)); err != nil {
		t.Fatalf("insert msg: %v", err)
	}

	// Member of the referencing conversation → 200.
	if rr := getMedia(s, mediaID, "u2"); rr.Code != http.StatusOK {
		t.Fatalf("member GET status = %d body=%s, want 200", rr.Code, rr.Body.String())
	}
}

// Unknown/blacklisted extensions are rejected (415); a whitelisted .pdf with
// a valid %PDF- header uploads and downloads.
func TestUploadWhitelist(t *testing.T) {
	s, _ := New(testDeps(t))

	// .bin is no longer a generic pass-through.
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "blob.bin", []byte("arbitrary-bytes"))))
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf(".bin status = %d, want 415", rr.Code)
	}

	// .pdf with a real PDF header is accepted and served back.
	pdf := append([]byte("%PDF-1.4\n"), []byte("%%EOF\n")...)
	rr = httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequest(t, "doc.pdf", pdf)))
	if rr.Code != http.StatusCreated {
		t.Fatalf(".pdf upload status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	gr := getMedia(s, resp.MediaID, testToken)
	if gr.Code != http.StatusOK {
		t.Fatalf(".pdf GET status = %d, want 200", gr.Code)
	}
}

// ------------------------------------------------- inline thumb/waveform tests

// TestUploadThumbAndWaveform: legal values are stored and echoed both in the
// upload response and in the GET metadata (?meta=1) view.
func TestUploadThumbAndWaveform(t *testing.T) {
	s, _ := New(testDeps(t))

	thumb := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
	wave := "[0,10,25,50,75,100,80,60]"
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequestMeta(t, "clip.ogg",
		append([]byte("OggS"), bytes.Repeat([]byte{0}, 64)...),
		map[string]string{"thumb_data": thumb, "waveform": wave})))
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload status = %d body=%s", rr.Code, rr.Body.String())
	}
	var up struct {
		MediaID   string `json:"media_id"`
		ThumbData string `json:"thumb_data"`
		Waveform  string `json:"waveform"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &up); err != nil {
		t.Fatalf("decode upload: %v", err)
	}
	if up.ThumbData != thumb || up.Waveform != wave {
		t.Fatalf("upload echo wrong: thumb=%q wave=%q", up.ThumbData, up.Waveform)
	}

	// GET metadata view returns the same fields.
	g := httptest.NewRequest(http.MethodGet, "/api/media/"+up.MediaID+"?meta=1", nil)
	gr := httptest.NewRecorder()
	s.Routes().ServeHTTP(gr, authed(g))
	if gr.Code != http.StatusOK {
		t.Fatalf("get meta status = %d", gr.Code)
	}
	if ct := gr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("meta content-type = %q", ct)
	}
	var meta struct {
		MediaID   string `json:"media_id"`
		ThumbData string `json:"thumb_data"`
		Waveform  string `json:"waveform"`
	}
	if err := json.Unmarshal(gr.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	if meta.ThumbData != thumb || meta.Waveform != wave {
		t.Fatalf("meta echo wrong: thumb=%q wave=%q", meta.ThumbData, meta.Waveform)
	}

	// Default GET (no meta) still streams the raw bytes.
	g2 := httptest.NewRequest(http.MethodGet, "/api/media/"+up.MediaID, nil)
	gr2 := httptest.NewRecorder()
	s.Routes().ServeHTTP(gr2, authed(g2))
	if gr2.Code != http.StatusOK || gr2.Header().Get("Content-Type") != "audio/ogg" {
		t.Fatalf("binary GET status=%d ct=%q", gr2.Code, gr2.Header().Get("Content-Type"))
	}
}

// TestUploadRejectsBadThumb: a thumbnail over 8KB is a 400.
func TestUploadRejectsBadThumb(t *testing.T) {
	s, _ := New(testDeps(t))
	huge := strings.Repeat("A", 8*1024+1) // one byte past the cap
	rr := httptest.NewRecorder()
	s.Routes().ServeHTTP(rr, authed(uploadRequestMeta(t, "p.png", pngBytes(t),
		map[string]string{"thumb_data": huge})))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rr.Code, rr.Body.String())
	}
}

// TestUploadRejectsBadWaveform: non-array / >32 elements / out-of-range → 400.
func TestUploadRejectsBadWaveform(t *testing.T) {
	s, _ := New(testDeps(t))
	cases := map[string]string{
		"object":     `{"a":1}`,
		"scalar":     `42`,
		"too-many":   "[" + strings.Repeat("1,", 40) + "1]",
		"over-range": `[0,50,101]`,
		"negative":   `[-1,50]`,
	}
	for name, wave := range cases {
		rr := httptest.NewRecorder()
		s.Routes().ServeHTTP(rr, authed(uploadRequestMeta(t, "p.png", pngBytes(t),
			map[string]string{"waveform": wave})))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (body=%s)", name, rr.Code, rr.Body.String())
		}
	}
}
