// Package adminapi 是 HTTP→帧翻译器（docs/bridge-design.md §2.2）：
// 唯一非 socket 入口。POST /admin/inject → 构造 Envelope → Pub 到 Bus。
//
// 禁令：不执行管理动作本身；JWT/TOTP 校验通过 Authorizer 接口
// 由 admin 层实现注入——本包只调用它。
package adminapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

var lg = log.For("adminapi")

// Authorizer 管理请求鉴权（JWT/TOTP），由 admin 层注入实现。
type Authorizer interface {
	Authorize(r *http.Request) error
}

// Publisher 由装配方注入的 Pub 能力（bus 侧）。
type Publisher interface {
	Pub(topic string, f *proto.Frame)
}

// API HTTP 翻译器。
type API struct {
	Auth Authorizer // 可 nil（embedded 模式本机信任）
	Pub  Publisher  // 必须非 nil

	injected uint64
}

// New 构造。
func New(auth Authorizer, pub Publisher) *API {
	return &API{Auth: auth, Pub: pub}
}

// InjectRequest POST /admin/inject 请求体。
type InjectRequest struct {
	Kind       uint32 `json:"kind"`
	ConvID     uint64 `json:"conv_id"`
	UserID     uint64 `json:"user_id"`
	PayloadB64 string `json:"payload_b64"`
}

// mux 内置路由（不依赖外部 mux；零逻辑装配由 main 挂接）。
func (a *API) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/inject", a.handleInject)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// Handler 返回 HTTP 处理器。
func (a *API) Handler() http.Handler { return a.mux() }

// ServeHTTP 直接实现 http.Handler。
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux().ServeHTTP(w, r) }

// handleInject HTTP → 帧 → Bus。
func (a *API) handleInject(w http.ResponseWriter, r *http.Request) {
	if a.Pub == nil {
		http.Error(w, "bus not wired", http.StatusServiceUnavailable)
		return
	}
	if a.Auth != nil {
		if err := a.Auth.Authorize(r); err != nil {
			lg.Warn("inject rejected", "err", err.Error())
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	var req InjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Kind == 0 || kinds.Layer(req.Kind) == "" || !kinds.IsInternal(req.Kind) && req.Kind >= 600 && req.Kind < 900 {
		http.Error(w, "invalid kind", http.StatusBadRequest)
		return
	}
	payload, err := base64.StdEncoding.DecodeString(req.PayloadB64)
	if err != nil {
		http.Error(w, "bad payload_b64: "+err.Error(), http.StatusBadRequest)
		return
	}
	env := chatpbEnvelope(req, payload)
	f := &proto.Frame{Kind: req.Kind, Env: env, Meta: &proto.FrameMeta{
		TraceID: "admin-inject", SrcLayer: "admin", EnqueuedAt: time.Now(),
	}}
	topic := topicFor(req)
	a.Pub.Pub(topic, f)
	atomic.AddUint64(&a.injected, 1)
	lg.Info("injected", "kind", kinds.KindName(req.Kind), "topic", topic)
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"topic": topic, "kind": req.Kind})
}

// Injected 已注入帧计数（观测）。
func (a *API) Injected() uint64 { return atomic.LoadUint64(&a.injected) }

func chatpbEnvelope(req InjectRequest, payload []byte) *chatpb.Envelope {
	env := &chatpb.Envelope{
		Kind:    req.Kind,
		Payload: payload,
	}
	if req.ConvID != 0 {
		env.SetConvId(req.ConvID)
	}
	if req.UserID != 0 {
		env.SetUserId(req.UserID)
	}
	return env
}

func topicFor(req InjectRequest) string {
	switch {
	case req.ConvID != 0:
		return "conv." + u64(req.ConvID)
	case req.UserID != 0:
		return "user." + u64(req.UserID)
	case req.Kind >= 500 && req.Kind < 600:
		return "sys.broadcast"
	default:
		return "sys.admin"
	}
}

func u64(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
