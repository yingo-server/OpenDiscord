// Package bridge 装配文件：唯一允许 import 全部组件包的地方
// （docs/bridge-design.md §2.2 验收标准 / CI import-lint 红线）。
// 链：framerw → verifier → router → bus → egress → framerw(写侧)；
// session/ack/supervisor 为旁路。
package bridge

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/bridge/ack"
	"github.com/yingo-server/UFOT/server/internal/bridge/adminapi"
	"github.com/yingo-server/UFOT/server/internal/bridge/egress"
	"github.com/yingo-server/UFOT/server/internal/bridge/framerw"
	"github.com/yingo-server/UFOT/server/internal/bridge/router"
	"github.com/yingo-server/UFOT/server/internal/bridge/session"
	"github.com/yingo-server/UFOT/server/internal/bridge/supervisor"
	"github.com/yingo-server/UFOT/server/internal/bridge/verifier"
	"github.com/yingo-server/UFOT/server/internal/bus"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/proto"
	chatpb "github.com/yingo-server/UFOT/server/internal/proto/gen/chatpb"
	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
	"github.com/yingo-server/UFOT/server/internal/transport"
)

// Config 装配配置（main 从 TOML 读入）。
type Config struct {
	LayerCaps  map[string][]kinds.Kind    // Verifier 能力表
	RouteTable map[uint32][]router.Target // Router 覆盖表；nil=默认
	// RouteFromData 是「来源层=data」时的精确路由覆盖（同 kind 双向转发用，
	// 如通话信令 350-354：上行 client→data，下行 data→user）。nil=无覆盖。
	RouteFromData map[uint32][]router.Target
	Egress        egress.Config // Egress 队列/水位
	// Sender 按层选择性发送（embedded 注入 Bridge.SendTo；nil 按 Peer.Layer 匹配）。
	Sender func(layer string, f *proto.Frame) error
	// AdminAuth 是 AdminAPI（POST /admin/inject）的入站鉴权。
	// nil = embedded 本机信任模式（只监听 UDS/loopback 时使用）。
	// 生产装配注入 adminapi.NewHMACAuthorizer(...)，见 AdminAPI()。
	AdminAuth adminapi.Authorizer

	// Signals 注入信号源。nil（默认）= supervisor 自己 signal.Notify 接管
	// SIGHUP/SIGTERM/SIGUSR1；非 nil = 由装配层统一分发（embedded 的 chatd 需要
	// 跨层排空顺序 usergw → pipe → bridge），此时传一个永不触发的空 channel，
	// supervisor 只响应 ctx 取消。
	Signals <-chan os.Signal
}

// Pipeline 一条完整桥接流水线 + 旁路组件。
type Pipeline struct {
	cfg  Config
	bus  *bus.Bus
	rtr  *router.Router
	eg   *egress.Egress
	sess *session.Manager
	sup  *supervisor.Supervisor

	peers  []transport.Peer
	rws    []*framerw.FrameRW
	conns  map[uint64]transport.Peer
	tracks map[string]*ack.Tracker // 每层一个（peer.Layer 索引）

	chVer    chan *proto.Frame // framerw → 控制面旁路
	chData   chan *proto.Frame // 控制面过滤后 → verifier
	chEgress chan *proto.Frame // verifier → route+pub → egress
	rejects  chan *proto.Reject
	runErr   chan error

	adminOnce sync.Once
	adminAPI  *adminapi.API
}

// Assemble 装配流水线。peers 为接入的各层（embedded 或 remote Peer）。
func Assemble(cfg Config, peers ...transport.Peer) (*Pipeline, error) {
	if len(peers) == 0 {
		return nil, fmt.Errorf("bridge: no peers")
	}
	if cfg.LayerCaps == nil {
		cfg.LayerCaps = defaultCaps()
	}
	p := &Pipeline{
		cfg: cfg, bus: bus.New(), rtr: router.New(cfg.RouteTable),
		eg: egress.New(cfg.Egress), sess: session.New(), sup: supervisor.New(),
		peers: peers, conns: map[uint64]transport.Peer{}, tracks: map[string]*ack.Tracker{},
		chVer: make(chan *proto.Frame, 256), chData: make(chan *proto.Frame, 256),
		chEgress: make(chan *proto.Frame, 256),
		rejects:  make(chan *proto.Reject, 64), runErr: make(chan error, 1),
	}
	p.cfg.Egress.Rejects = p.rejects
	for k, ts := range cfg.RouteFromData {
		p.rtr.SetFromData(k, ts)
	}
	p.cfg.Egress.OnNack = func(f *proto.Frame) {
		for _, tr := range p.tracks {
			tr.Fail(f.Seq)
		}
	}

	// 旁路：SUBSCRIBE → bus 订阅并回投该连接；心跳判死 → 断连。
	p.sess.OnSubscribe = func(id uint64, topic string) {
		ch, _ := p.bus.Sub(topic, "bridge")
		go func() {
			for f := range ch {
				if pr, ok := p.conns[id]; ok {
					if rw := p.rwOf(pr.Layer()); rw != nil {
						_ = rw.Write(f)
					}
				}
			}
		}()
	}
	p.sess.OnDead = func(id uint64) {
		if pr, ok := p.conns[id]; ok {
			_ = pr.Close()
		}
	}
	for _, peer := range peers {
		rw := framerw.New(peer, p.chVer)
		connID := p.sess.Register(peer, peer.Layer(), cfg.LayerCaps[peer.Layer()])
		rw.ConnID = connID
		p.conns[connID] = peer
		p.rws = append(p.rws, rw)
		tr := ack.New()
		tr.Resend = func(_ uint32, f *proto.Frame) { _ = p.eg.Enqueue(p.route(f), f) }
		p.tracks[peer.Layer()] = tr
		p.sup.AddDrain("peer:"+peer.Layer(), func(context.Context) { rw.Close() })
	}
	p.sup.AddDrain("egress", p.eg.DrainWithContext)
	p.sup.OnShutdown = func() {
		f := &proto.Frame{Kind: kinds.KindShutdownNotice,
			Env:  &chatpb.Envelope{Kind: uint32(kinds.KindShutdownNotice)},
			Meta: &proto.FrameMeta{TraceID: "shutdown", SrcLayer: "bridge", EnqueuedAt: time.Now()}}
		p.bus.Pub("sys.shutdown", f)
	}
	p.sup.AddSnapshot(func() {
		lg.Info("dump", "bus", fmt.Sprintf("%+v", p.bus.Stats()), "egress", fmt.Sprintf("%+v", p.eg.Stats()))
	})
	return p, nil
}

// Run 启动全链。阻塞直至 ctx 结束或致命错误。
func (p *Pipeline) Run(ctx context.Context) error {
	for _, rw := range p.rws {
		go func(r *framerw.FrameRW) {
			if err := r.Run(ctx); err != nil && err != context.Canceled {
				lg.Warn("framerw exited", "err", err.Error())
			}
		}(rw)
	}
	v := verifier.New(p.chData, p.chEgress, p.rejects, p.cfg.LayerCaps)
	go func() { _ = v.Run(ctx) }()
	go p.controlLoop(ctx)
	go p.dispatchLoop(ctx)
	go p.writeLoop(ctx)
	go p.rejectLoop(ctx)
	go p.sweepLoop(ctx)

	code, err := p.sup.Run(ctx, p.cfg.Signals)
	if err != nil && code == 0 {
		return err
	}
	return p.runErrAwait(code)
}

// controlLoop：控制面旁路（HELLO/SUBSCRIBE/ACK/HEARTBEAT 不进流水线）。
func (p *Pipeline) controlLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-p.chVer:
			if !ok {
				return
			}
			if p.sess.ConsumeControl(connOf(f), f) {
				continue
			}
			select {
			case p.chData <- f:
			case <-ctx.Done():
				return
			}
		}
	}
}

// dispatchLoop：校验后 → 控制面旁路 → Router → bus 观测扇出 + egress 投递。
func (p *Pipeline) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-p.chEgress:
			if !ok {
				return
			}
			if p.sess.ConsumeControl(connOf(f), f) {
				continue
			}
			targets := p.rtr.RouteFrom(f.Kind, f.Env, frameSrcLayer(f))
			if len(targets) == 0 {
				select {
				case p.rejects <- &proto.Reject{Kind: f.Kind, Code: 4, Reason: "no route"}:
				default:
				}
				continue
			}
			for _, t := range targets {
				p.bus.Pub(t.Topic, f)
				if err := p.eg.Enqueue(t, f); err != nil {
					lg.Warn("egress enqueue failed", "layer", t.Layer, "kind", kinds.KindName(f.Kind), "err", err.Error())
				}
			}
		}
	}
}

// writeLoop：egress out → 目标层 Peer.Send（FrameRW 写侧）。
func (p *Pipeline) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case tf, ok := <-p.eg.Out():
			if !ok {
				return
			}
			if err := p.sendTo(tf.T.Layer, tf.F); err != nil {
				lg.Warn("peer send failed", "layer", tf.T.Layer, "err", err.Error())
			}
		}
	}
}

// route 从帧推导首要目标（ack 重发用）。
func (p *Pipeline) route(f *proto.Frame) router.Target {
	if ts := p.rtr.RouteFrom(f.Kind, f.Env, frameSrcLayer(f)); len(ts) > 0 {
		return ts[0]
	}
	return router.Target{}
}

// frameSrcLayer reads the ingress layer stamped by framerw ("" when unknown).
func frameSrcLayer(f *proto.Frame) string {
	if f != nil && f.Meta != nil {
		return f.Meta.SrcLayer
	}
	return ""
}

// sendTo 目标层写侧发送；层未接入即丢弃（TARGET_DOWN 语义）。
func (p *Pipeline) sendTo(layer string, f *proto.Frame) error {
	if tr := p.tracks[layer]; tr != nil {
		tr.Track(f.Seq, f)
	}
	if p.cfg.Sender != nil {
		return p.cfg.Sender(layer, f)
	}
	for i, peer := range p.peers {
		if peer.Layer() == layer {
			return p.rws[i].Write(f)
		}
	}
	return nil
}

// rejectLoop：NACK/无路由仅记日志（降级不崩；重发由 AckTracker 覆盖）。
func (p *Pipeline) rejectLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r, ok := <-p.rejects:
			if !ok {
				return
			}
			lg.Warn("reject", "kind", kinds.KindName(r.Kind), "code", fmt.Sprint(r.Code), "reason", r.Reason)
		}
	}
}

// sweepLoop 周期维护 session（心跳 60s / 租约 30s）与 ack RTO（300ms）。
func (p *Pipeline) sweepLoop(ctx context.Context) {
	tk := time.NewTicker(100 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			p.sess.Sweep()
			for _, tr := range p.tracks {
				tr.Tick()
			}
		}
	}
}

// Drain 优雅排空。
func (p *Pipeline) Drain(ctx context.Context) { p.sup.Drain(ctx) }

// SetRoute 覆盖/追加一条 kind 的投递规则（SIGHUP 白名单：路由表热更）。
// 装配层用它补齐 101→data、103→user 这类精确路由。
func (p *Pipeline) SetRoute(kind uint32, targets []router.Target) { p.rtr.Set(kind, targets) }

// Snapshot 返回总线/egress 的观测快照（SIGUSR1 状态 dump、指标采集）。
func (p *Pipeline) Snapshot() map[string]string {
	return map[string]string{
		"bus":    fmt.Sprintf("%+v", p.bus.Stats()),
		"egress": fmt.Sprintf("%+v", p.eg.Stats()),
	}
}

// AdminAPI 返回与本流水线绑定的 AdminAPI handler（POST /admin/inject 是唯一
// 非 socket 入口）。鉴权来自 Config.AdminAuth：nil 即 embedded 本机信任模式。
// 首次调用时构造，之后复用同一个 handler/计数器。
func (p *Pipeline) AdminAPI() *adminapi.API {
	p.adminOnce.Do(func() { p.adminAPI = adminapi.New(p.cfg.AdminAuth, p) })
	return p.adminAPI
}

// Pub 实现 adminapi.Publisher：管理面 HTTP 注入唯一入口。
func (p *Pipeline) Pub(topic string, f *proto.Frame) {
	if f == nil {
		return
	}
	if f.Meta == nil {
		f.Meta = &proto.FrameMeta{}
	}
	if f.Meta.SrcLayer == "" {
		f.Meta.SrcLayer = "admin"
	}
	if f.Meta.TraceID == "" {
		f.Meta.TraceID = "admin-inject"
	}
	if f.Meta.EnqueuedAt.IsZero() {
		f.Meta.EnqueuedAt = time.Now()
	}
	select {
	case p.chVer <- f:
	case <-time.After(200 * time.Millisecond):
		lg.Warn("admin inject dropped", "topic", topic)
	}
}

// rwOf 按层查写侧 FrameRW（订阅回投用）。
func (p *Pipeline) rwOf(layer string) *framerw.FrameRW {
	for _, rw := range p.rws {
		if rw.Peer.Layer() == layer {
			return rw
		}
	}
	return nil
}

func connOf(f *proto.Frame) uint64 {
	if f != nil && f.Meta != nil {
		return f.Meta.ConnID
	}
	return 0
}

func (p *Pipeline) runErrAwait(code supervisor.ExitCode) error {
	if code != 0 {
		return fmt.Errorf("bridge: exit code %d", code)
	}
	select {
	case err := <-p.runErr:
		return err
	default:
		return nil
	}
}

var lg = log.For("bridge")
