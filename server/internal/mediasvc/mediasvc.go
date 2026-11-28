// Package mediasvc implements the 1.0 "good enough" media plane: a small HTTP
// upload/download service mounted on the same user-facing port as authsvc
// (chatd buildHTTP mux). Media bytes never travel the WS channel — the frame
// codec caps a frame at 64KB (frame.MaxFrameBytes), so images/files/voice go
// through this REST surface instead. The full design (presigned direct
// upload, thumbnail transcoding, waveform, per-conversation ACL) is 1.1 scope.
//
// Endpoints (both behind the access_token cookie, same as authsvc):
//
//	POST /api/media/upload  multipart/form-data, field "file"
//	GET  /api/media/{id}    streamed bytes
//
// Storage layout: {media_dir}/{sha256[:2]}/{sha256[2:4]}/{sha256} — content
// addressed, so identical uploads dedupe on disk. media_id adds 8 random
// bytes on top of sha256[:16] so the DB key cannot be derived from the file
// path alone (docs/security-spec §4.3: 对象路径不可推导).
package mediasvc

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"database/sql"

	"github.com/yingo-server/UFOT/server/internal/data/store"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

// mtype numbers mirror chatpb.Media semantics: 1 image, 2 audio, 3 video,
// 4 file. 1.0 accepts image/audio/file (video stays 1.1).
const (
	mtypeImage = 1
	mtypeAudio = 2
	mtypeFile  = 4
)

// size caps (security-spec §4.3 MVP cut: image 8MB / voice 2MB / file 20MB).
const (
	maxImageBytes = 8 << 20
	maxAudioBytes = 2 << 20
	maxFileBytes  = 20 << 20
)

// dailyUploadLimit is the per-user fixed-window cap (sys_rate_limit lane).
const dailyUploadLimit = 50

// Storage quotas (LTS admission: the media plane must be bounded). Both are
// overridable via Deps.Quota; these are the defaults.
const (
	defaultUserQuotaBytes  = 500 << 20 // 500MB per user
	defaultGlobalQuotaByte = 20 << 30  // 20GB across all users
)

// magic rules: (name, offset, prefix, allowed mtype, server mime). WebP's
// RIFF header has 4 don't-care bytes between "RIFF" and "WEBP".
type magicRule struct {
	prefix []byte
	offset int
	mtype  int
	mime   string
}

var magicTable = []magicRule{
	// 前 6 种抄自 docs/security-spec.md §4.3 魔数表（JPEG/PNG/WEBP/GIF/OGG/MP3）。
	{[]byte{0xFF, 0xD8, 0xFF}, 0, mtypeImage, "image/jpeg"},
	{[]byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, 0, mtypeImage, "image/png"},
	{[]byte("WEBP"), 8, mtypeImage, "image/webp"},
	{[]byte("GIF87a"), 0, mtypeImage, "image/gif"},
	{[]byte("GIF89a"), 0, mtypeImage, "image/gif"},
	{[]byte("OggS"), 0, mtypeAudio, "audio/ogg"},
	{[]byte("ID3"), 0, mtypeAudio, "audio/mpeg"},
	{[]byte{0xFF, 0xFB}, 0, mtypeAudio, "audio/mpeg"},
}

// blockedExt rejects executable/script containers outright (spec §4.3 红线:
// 可执行/脚本拒绝; SVG 是 XSS 向量, 不接受).
var blockedExt = map[string]bool{
	".exe": true, ".sh": true, ".bat": true, ".cmd": true, ".ps1": true,
	".js": true, ".mjs": true, ".hta": true, ".jar": true, ".apk": true,
	".dll": true, ".scr": true, ".com": true, ".pif": true, ".svg": true,
	".html": true, ".htm": true, ".php": true, ".py": true, ".pl": true,
}

// TokenVerifier is the access-token check surface (satisfied by
// *authn.Verifier; an interface so tests can fake it).
type TokenVerifier interface {
	Verify(token string) (*authn.Identity, error)
} // Quota is the storage cap set (bytes). Zero fields fall back to the
// defaults (500MB per user / 20GB global). Over-quota uploads answer 413.
type Quota struct {
	UserBytes   int64 // per-user total SUM(size)
	GlobalBytes int64 // total across all users
}

// Deps is the injected dependency set (assembly wires the same hot DB handle
// the rest of the user plane uses).
type Deps struct {
	Dir      string // media_dir from config
	Verifier TokenVerifier
	Media    store.MediaStore
	Sys      store.SysStore
	// Quota is the optional quota accounting handle (*sqlite.DB satisfies
	// store.MediaQuota). Nil disables quota enforcement (tests).
	Quota store.MediaQuota
	// QuotaLimits overrides the default caps; zero fields use the defaults.
	QuotaLimits Quota
	// Conv resolves conversation membership for the GET ACL (security-spec §4.4).
	Conv store.ConvStore
	// QueryDB is the hot DB used to find which conversations reference a
	// media_id (msg_messages_hot content scan). May be nil in unit fakes.
	QueryDB *sql.DB
	Logger  *log.Logger
	// Now is overridable in tests.
	Now func() time.Time
}

// Service is the media HTTP handler.
type Service struct {
	deps Deps
}

// New validates deps and returns the service.
func New(d Deps) (*Service, error) {
	if d.Dir == "" {
		return nil, errors.New("mediasvc: media dir is required")
	}
	if d.Verifier == nil || d.Media == nil || d.Sys == nil {
		return nil, errors.New("mediasvc: verifier, media store and sys store are required")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = log.For("mediasvc")
	}
	if err := os.MkdirAll(d.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("mediasvc: mkdir %s: %w", d.Dir, err)
	}
	return &Service{deps: d}, nil
}

// Routes mounts on the shared user mux: `mux.Handle("/api/media/", svc.Routes())`.
func (s *Service) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/media/upload", s.handleUpload)
	m.HandleFunc("GET /api/media/{id}", s.handleGet)
	return m
}

// authenticate shares the authsvc contract: access_token HttpOnly cookie only
// (never the URL — security-spec §2.3).
func (s *Service) authenticate(r *http.Request) (*authn.Identity, bool) {
	c, err := r.Cookie("access_token")
	if err != nil || c.Value == "" {
		return nil, false
	}
	id, err := s.deps.Verifier.Verify(c.Value)
	if err != nil || id == nil || id.UserID <= 0 {
		return nil, false
	}
	return id, true
}

func (s *Service) unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "code": "UNAUTHORIZED"})
}

// @authz: checked — upload binds the row to the cookie uid; GET is guarded by
// unguessable media_id only (no conversation ACL yet, see known-issues 1.0).
func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authenticate(r)
	if !ok {
		s.unauthorized(w)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid multipart form", "code": "BAD_REQUEST"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing file field", "code": "BAD_REQUEST"})
		return
	}
	defer file.Close()

	// Filename is metadata only: strip path, cap length (spec §4.3 文件名清洗).
	name := filepath.Base(header.Filename)
	name = strings.TrimSpace(name)
	if len(name) > 255 {
		name = name[:255]
	}

	// Read fully (caps are ≤20MB, bounded) so magic + sha256 run on the bytes.
	limit := maxFileBytes + 1
	body, err := io.ReadAll(io.LimitReader(file, int64(limit)))
	if err != nil || len(body) > maxFileBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "file too large", "code": "FILE_TOO_LARGE"})
		return
	}

	// Optional inline metadata (multipart form fields): a base64 thumbnail and
	// a JSON waveform array. Both are validated before the row is written;
	// an invalid value is a 400 and nothing is persisted.
	thumbData := r.FormValue("thumb_data")
	waveform := r.FormValue("waveform")
	if err := validateThumbWaveform(thumbData, waveform); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "BAD_REQUEST"})
		return
	}

	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	mtype, mime, err := classify(name, body)
	if err != nil {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": err.Error(), "code": "UNSUPPORTED_TYPE"})
		return
	}
	max := capFor(mtype)
	if int64(len(body)) > max {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "file too large", "code": "FILE_TOO_LARGE"})
		return
	}

	// Per-user daily cap via sys_rate_limit (50 uploads/day fixed window).
	if err := s.checkRate(r.Context(), id.UserID); err != nil {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "daily upload limit reached", "code": "RATE_LIMITED"})
		return
	}

	// Storage quota (LTS admission): per-user and global caps on SUM(size).
	// An over-quota upload is rejected with 413 — the media plane must be
	// bounded or the disk grows without limit.
	if err := s.checkQuota(r.Context(), id.UserID, int64(len(body))); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error": "storage quota exceeded", "code": "QUOTA_EXCEEDED"})
		return
	}

	mediaID, err := newMediaID(sha)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error", "code": "INTERNAL"})
		return
	}
	if err := s.storeFile(sha, body); err != nil {
		s.deps.Logger.Error("media write failed", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error", "code": "INTERNAL"})
		return
	}

	// Thumbnail bytes go to a FILE, not the DB (migrations/hot/media/0003):
	// an inline base64 blob cost ~10.7KB of TEXT per row and ~10.7GB at a
	// million images. The row stores only the path (tens of bytes). A write
	// failure degrades to NO thumbnail (not a failed upload): the preview is
	// cosmetic, the media is the payload.
	thumbPath := ""
	if thumbData != "" {
		thumbPath = s.thumbPathFor(sha)
		if err := s.storeThumbFile(thumbPath, []byte(thumbData)); err != nil {
			s.deps.Logger.Warn("thumb file write failed; serving without inline preview", "err", err.Error())
			thumbPath = ""
		}
	}

	// MVP: no async scanner is wired, so the row is born usable (status=1).
	// The 1.1 ClamAV pipeline will insert status=0 and flip on scan pass.
	rec := &store.Media{
		MediaID:   mediaID,
		UID:       id.UserID,
		MType:     mtype,
		Name:      name,
		Size:      int64(len(body)),
		Mime:      mime,
		Status:    1,
		SHA256:    sha,
		CreatedAt: s.deps.Now().Unix(),
		// ThumbData stays EMPTY for new rows (the file carries the bytes);
		// ThumbPath is the on-disk location. Legacy rows with a non-empty
		// ThumbData are served via the read-time fallback. The upload ECHO
		// still returns the thumbnail value (the client already has it —
		// no extra round-trip), which is why thumbEcho is passed separately.
		ThumbData: "",
		ThumbPath: thumbPath,
		Waveform:  waveform,
	}
	if err := s.deps.Media.Insert(r.Context(), rec); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			// Same content uploaded twice from this account: fine, hand back
			// the fresh id (path is content-addressed, bytes already there).
			writeJSON(w, http.StatusOK, uploadOKBody(mediaID, rec, thumbData))
			return
		}
		if p, perr := s.pathFor(sha); perr == nil {
			_ = os.Remove(p)
		}
		s.deps.Logger.Error("media insert failed", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error", "code": "INTERNAL"})
		return
	}

	writeJSON(w, http.StatusCreated, uploadOKBody(mediaID, rec, thumbData))
}

// uploadOKBody is the shared upload response shape. thumbEcho is the
// thumbnail payload the client just sent (echoed so it can render without a
// second GET); waveform likewise. Only when present.
func uploadOKBody(mediaID string, rec *store.Media, thumbEcho string) map[string]any {
	body := map[string]any{
		"media_id": mediaID, "size": rec.Size, "mime": rec.Mime, "mtype": rec.MType,
	}
	if thumbEcho != "" {
		body["thumb_data"] = thumbEcho
	}
	if rec.Waveform != "" {
		body["waveform"] = rec.Waveform
	}
	return body
}

// @authz: checked — media_id → msg → cid → membership (canReadMedia,
// security-spec §4.4). The uploader always passes; anyone else must share a
// conversation that references this media. Non-members get 404 (not 403) so
// the endpoint cannot be used to probe which media ids exist.
func (s *Service) handleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := s.authenticate(r)
	if !ok {
		s.unauthorized(w)
		return
	}
	mediaID := r.PathValue("id")
	if !validMediaID(mediaID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "NOT_FOUND"})
		return
	}
	rec, err := s.deps.Media.Get(r.Context(), mediaID)
	if err != nil || rec == nil || rec.Status != 1 {
		// 存在性不区分：无权限与不存在同响应（spec §4.4）。
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "NOT_FOUND"})
		return
	}
	// 会话 ACL：上传者直通；否则仅当请求者是引用该 media_id 的某条消息
	// 所在会话的成员时放行。都不中 → 404（不是 403，防枚举，spec §4.4）。
	if rec.UID != id.UserID && !s.canReadMedia(r.Context(), mediaID, id.UserID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "NOT_FOUND"})
		return
	}
	// Metadata view: when the client asks for JSON (Accept: application/json or
	// ?meta=1) return the record incl. thumb_data/waveform so it need not issue
	// a second request. The default path below still streams the raw bytes, so
	// image/audio/download behaviour is untouched.
	if wantsJSON(r) {
		// Thumbnail resolution order (0003 migration): the FILE first; when
		// the file is missing (or the row predates the migration), fall back
		// to the legacy inline thumb_data column — existing rows must keep
		// rendering, never break.
		thumb := ""
		if rec.ThumbPath != "" {
			if p, perr := s.absThumbPath(rec.ThumbPath); perr == nil {
				if b, err := os.ReadFile(p); err == nil {
					thumb = string(b)
				}
			}
		}
		if thumb == "" {
			thumb = rec.ThumbData // legacy inline fallback (or empty)
		}
		body := map[string]any{
			"media_id": rec.MediaID, "size": rec.Size, "mime": rec.Mime,
			"mtype": rec.MType, "name": rec.Name,
			"thumb_data": thumb, "waveform": rec.Waveform,
		}
		writeJSON(w, http.StatusOK, body)
		return
	}
	path, err := s.pathFor(rec.SHA256)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "NOT_FOUND"})
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found", "code": "NOT_FOUND"})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", rec.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", urlEscape(rec.Name)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, rec.Name, time.Unix(rec.CreatedAt, 0), f)
}

// ---------------------------------------------------------------- internals

// canReadMedia reports whether uid is a member of some conversation that
// references mediaID in a stored message. The reference scan is a LIKE on the
// message content BLOB; media_id chars are escaped so `_`/`%`/`\` are literal.
func (s *Service) canReadMedia(ctx context.Context, mediaID string, uid int64) bool {
	if s.deps.Conv == nil || s.deps.QueryDB == nil {
		return false
	}
	pattern := "%" + escapeLike(mediaID) + "%"
	rows, err := s.deps.QueryDB.QueryContext(ctx,
		`SELECT DISTINCT cid FROM msg_messages_hot WHERE content LIKE ? ESCAPE '\'`, pattern)
	if err != nil {
		s.deps.Logger.Warn("media ACL: reference scan failed", "err", err.Error())
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int64
		if err := rows.Scan(&cid); err != nil {
			continue
		}
		if m, err := s.deps.Conv.CheckMember(ctx, cid, uid); err == nil && m != nil {
			return true
		}
	}
	return false
}

// escapeLike escapes LIKE metacharacters with a backslash so the media_id is
// matched literally (ESCAPE '\').
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// classify enforces the triple check: magic bytes → inferred type, declared
// extension whitelist, and the mtype cap. A mismatch rejects (spec §4.3 红线).
func classify(name string, body []byte) (mtype int, mime string, err error) {
	ext := strings.ToLower(filepath.Ext(name))
	if blockedExt[ext] {
		return 0, "", errors.New("file type not allowed")
	}
	if len(body) == 0 {
		return 0, "", errors.New("empty file")
	}
	for _, rule := range magicTable {
		off := rule.offset
		if off+len(rule.prefix) > len(body) {
			continue
		}
		if string(body[off:off+len(rule.prefix)]) != string(rule.prefix) {
			continue
		}
		// Magic matched. The extension must agree when it is a known one.
		switch rule.mtype {
		case mtypeImage:
			if ext != "" && ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".webp" && ext != ".gif" {
				return 0, "", errors.New("extension does not match file content")
			}
		case mtypeAudio:
			if ext != "" && ext != ".ogg" && ext != ".oga" && ext != ".opus" && ext != ".mp3" {
				return 0, "", errors.New("extension does not match file content")
			}
		}
		return rule.mtype, rule.mime, nil
	}
	// Executable / script headers are rejected outright, even if the declared
	// extension is otherwise allowed (MZ = PE/COFF, \x7fELF, #! = shebang).
	if hasExecHeader(body) {
		return 0, "", errors.New("file type not allowed")
	}
	// Not on the magic table → generic file. 1.0 only trusts a fixed extension
	// whitelist (spec §4.3 红线: 任意扩展名不再放行, 未知类型 415).
	if ext == "" {
		return 0, "", errors.New("unknown file type")
	}
	mime, ok := fileExtMime[ext]
	if !ok {
		return 0, "", errors.New("file type not allowed")
	}
	return mtypeFile, mime, nil
}

// hasExecHeader reports whether body opens with an executable/script magic.
func hasExecHeader(body []byte) bool {
	if len(body) >= 2 && body[0] == 'M' && body[1] == 'Z' {
		return true
	}
	if len(body) >= 4 && body[0] == 0x7f && body[1] == 'E' && body[2] == 'L' && body[3] == 'F' {
		return true
	}
	if len(body) >= 2 && body[0] == '#' && body[1] == '!' {
		return true
	}
	return false
}

// fileExtMime is the 1.0 generic-file whitelist (document/archive types only).
var fileExtMime = map[string]string{
	".pdf":  "application/pdf",
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".csv":  "text/csv; charset=utf-8",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".zip":  "application/zip",
}

func capFor(mtype int) int64 {
	switch mtype {
	case mtypeImage:
		return maxImageBytes
	case mtypeAudio:
		return maxAudioBytes
	default:
		return maxFileBytes
	}
}

func (s *Service) checkRate(ctx context.Context, uid int64) error {
	day := s.deps.Now().UTC().Truncate(24 * time.Hour).Unix()
	w, err := s.deps.Sys.RateLimitIncr(ctx, fmt.Sprintf("u:%d", uid), "media.upload", day)
	if err != nil {
		// Counter failure must not brick uploads; log and allow.
		s.deps.Logger.Warn("media rate counter failed", "err", err.Error())
		return nil
	}
	if w != nil && w.Count > dailyUploadLimit {
		return errors.New("rate limited")
	}
	return nil
}

func (s *Service) pathFor(sha string) (string, error) {
	if len(sha) < 4 {
		return "", fmt.Errorf("mediasvc: bad sha %q", sha)
	}
	return safeJoin(s.deps.Dir, filepath.Join(sha[:2], sha[2:4], sha))
}

// thumbPathFor is the RELATIVE (to media_dir) path of the thumbnail file:
// thumbs/<sha[:2]>/<sha[:4]>/<sha>.thumb — same sharding as the media bytes,
// stored in its own subtree so ops can wipe/copy thumbs independently.
func (s *Service) thumbPathFor(sha string) string {
	return filepath.Join("thumbs", sha[:2], sha[:4], sha+".thumb")
}

// absThumbPath maps the stored relative thumb path to the on-disk absolute
// path. The stored value is normally written by this service, but it may come
// from a legacy/edited DB row, so the join is guarded by safeJoin: a value
// that is absolute or escapes media_dir is refused rather than resolved.
func (s *Service) absThumbPath(rel string) (string, error) {
	return safeJoin(s.deps.Dir, rel)
}

// safeJoin resolves rel under base and refuses anything that could escape the
// base directory. It rejects: absolute paths, any ".." path component, and a
// cleaned result whose prefix is not base (defense in depth — a symlinked base
// or a platform separator quirk must never let an attacker-controlled path
// reach a file outside media_dir).
func safeJoin(base, rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", fmt.Errorf("mediasvc: empty relative path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("mediasvc: absolute path not allowed: %q", rel)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("mediasvc: path escapes base (..): %q", rel)
		}
	}
	baseClean := filepath.Clean(base)
	joined := filepath.Join(baseClean, rel)
	if joined != baseClean && !strings.HasPrefix(joined, baseClean+string(filepath.Separator)) {
		return "", fmt.Errorf("mediasvc: path escapes base: %q", rel)
	}
	return joined, nil
}

// storeThumbFile writes the thumbnail payload (the raw validated value,
// base64 text) to path atomically: temp file + rename, same as storeFile.
// Content-addressed by sha, so identical images dedupe.
func (s *Service) storeThumbFile(rel string, payload []byte) error {
	path, err := s.absThumbPath(rel)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // content-addressed dedupe
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".thumb-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// checkQuota enforces the per-user and global storage caps. incoming is the
// size of the upload about to be stored. A nil Quota dep disables the check
// (unit tests without a DB); a counter failure is logged and the upload
// proceeds (a quota counter outage must not brick the media plane — the same
// leniency as checkRate).
func (s *Service) checkQuota(ctx context.Context, uid int64, incoming int64) error {
	q := s.deps.Quota
	if q == nil {
		return nil
	}
	userCap := s.deps.QuotaLimits.UserBytes
	if userCap <= 0 {
		userCap = defaultUserQuotaBytes
	}
	globalCap := s.deps.QuotaLimits.GlobalBytes
	if globalCap <= 0 {
		globalCap = defaultGlobalQuotaByte
	}
	if userCap > 0 {
		used, err := q.UsageOf(ctx, uid)
		if err != nil {
			s.deps.Logger.Warn("quota usage query failed; allowing upload", "err", err.Error())
			return nil
		}
		if used+incoming > userCap {
			s.deps.Logger.Info("user media quota exceeded", "uid", uid, "used", used, "cap", userCap)
			return errors.New("user quota exceeded")
		}
	}
	if globalCap > 0 {
		used, err := q.TotalUsage(ctx)
		if err != nil {
			s.deps.Logger.Warn("quota total query failed; allowing upload", "err", err.Error())
			return nil
		}
		if used+incoming > globalCap {
			s.deps.Logger.Info("global media quota exceeded", "used", used, "cap", globalCap)
			return errors.New("global quota exceeded")
		}
	}
	return nil
}

func (s *Service) storeFile(sha string, body []byte) error {
	path, err := s.pathFor(sha)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // content-addressed dedupe
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// newMediaID = base64url(sha256[:16] ‖ 8 random bytes). The random tail makes
// the DB key non-derivable from the object path (spec §4.3), and the result
// is 32 chars of URL-safe alphabet.
func newMediaID(shaHex string) (string, error) {
	raw, err := hex.DecodeString(shaHex[:32]) // sha256[:16]
	if err != nil {
		return "", err
	}
	var tail [8]byte
	if _, err := rand.Read(tail[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(append(raw, tail[:]...)), nil
}

func validMediaID(id string) bool {
	if len(id) < 22 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x21 || r > 0x7e || r == '"' || r == '\\' || r == '%' {
			continue // strip, do not encode: filename is metadata only
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "file"
	}
	return b.String()
}

// ------------------------------------------------------------------ inline metadata

const (
	maxThumbBytes     = 8 << 10 // 8KB cap on the inline thumbnail
	maxWaveformPoints = 32      // 24 expected; tolerate up to 32
)

// validateThumbWaveform enforces the optional inline-metadata contract:
//   - thumb_data ≤ 8KB (empty allowed);
//   - waveform, when present, must be a JSON ARRAY of ≤32 integers in 0-100.
func validateThumbWaveform(thumb, wave string) error {
	if len(thumb) > maxThumbBytes {
		return errors.New("thumb_data too large (max 8KB)")
	}
	if err := validateThumbPayload(thumb); err != nil {
		return err
	}
	if wave == "" {
		return nil
	}
	trimmed := strings.TrimSpace(wave)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return errors.New("waveform must be a JSON array")
	}
	var arr []float64
	if err := json.Unmarshal([]byte(trimmed), &arr); err != nil {
		return errors.New("waveform must be a JSON array of numbers")
	}
	if len(arr) > maxWaveformPoints {
		return errors.New("waveform too many points (max 32)")
	}
	for _, v := range arr {
		if v != math.Trunc(v) || v < 0 || v > 100 {
			return errors.New("waveform values must be integers 0-100")
		}
	}
	return nil
}

// validateThumbPayload checks that thumb_data is an inline image we are willing
// to store and later render. Accepts a bare base64 body or a data: URI, but
// ONLY for raster formats (JPEG/PNG/WEBP/GIF). An attacker-supplied
// data:image/svg+xml or data:text/html would be inert in <img> today, yet it
// is a stored XSS payload the moment any consumer renders it as HTML.
func validateThumbPayload(thumb string) error {
	if thumb == "" {
		return nil
	}
	raw := thumb
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		comma := strings.IndexByte(raw, ',')
		if comma < 0 {
			return errors.New("thumb_data data URI is malformed")
		}
		meta := strings.ToLower(raw[:comma])
		switch {
		case strings.Contains(meta, "svg"), strings.Contains(meta, "xml"),
			strings.Contains(meta, "html"), strings.Contains(meta, "javascript"):
			return errors.New("thumb_data may only be a raster image (jpeg/png/webp/gif)")
		}
		raw = raw[comma+1:]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("thumb_data is empty")
	}
	dec, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		// tolerate URL-safe / unpadded variants
		dec, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(raw, "="))
		if err != nil {
			return errors.New("thumb_data must be valid base64")
		}
	}
	if len(dec) < 12 {
		return errors.New("thumb_data payload too short to be an image")
	}
	switch {
	case bytes.HasPrefix(dec, []byte{0xFF, 0xD8, 0xFF}):
	case bytes.HasPrefix(dec, []byte{0x89, 'P', 'N', 'G'}):
	case bytes.HasPrefix(dec, []byte("GIF8")):
	case len(dec) >= 12 && string(dec[0:4]) == "RIFF" && string(dec[8:12]) == "WEBP":
	default:
		return errors.New("thumb_data must be a JPEG/PNG/WEBP/GIF image")
	}
	return nil
}

// wantsJSON reports whether the GET caller asked for the metadata view rather
// than the raw bytes (?meta=1 or Accept: application/json).
func wantsJSON(r *http.Request) bool {
	if r.URL.Query().Get("meta") == "1" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":"internal error","code":"INTERNAL"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
