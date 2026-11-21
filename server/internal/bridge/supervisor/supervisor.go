// Package supervisor 负责信号→事件翻译与生命周期（docs/bridge-design.md §2.2/§5）：
// SIGHUP→Reload 回调；SIGTERM→广播 ShutdownNotice→各组件 Drain(5s)→退出码 0。
// 使用 pkg/signalset，本包与 signalset 是全仓唯二可 import os/signal 的包。
//
// 禁令：不碰任何帧（ShutdownNotice 的构造与广播由装配方回调完成）。
package supervisor

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/signalset"
)

// DrainTimeout SIGTERM 优雅排空上限。
const DrainTimeout = 5 * time.Second

// ExitCode 返回给 main 的退出码（0 优雅、70 不变量破坏）。
type ExitCode int

const (
	ExitOK           ExitCode = 0
	ExitInvariant    ExitCode = 70 // AckTracker panic 由 Recover 转
	ExitConfigError  ExitCode = 1
	ExitAssembleFail ExitCode = 2
)

// Supervisor 生命周期管理器。
type Supervisor struct {
	set *signalset.Set

	mu      sync.Mutex
	drains  []namedDrain
	reloads []func()
	panics  []func() // panic 副作用回调（如快照 dump）

	OnReload   func() // SIGHUP 语义回调（热重载白名单）
	OnShutdown func() // SIGTERM：广播 ShutdownNotice（由装配方构造帧）
	OnPanic    func() // 不变量破坏时的快照回调
}

type namedDrain struct {
	name string
	fn   func(ctx context.Context)
}

// New 构造。
func New() *Supervisor {
	return &Supervisor{set: signalset.NewSet()}
}

// AddDrain 注册一个组件的排空函数（SIGTERM 时按注册序执行，ctx 5s 上限）。
func (s *Supervisor) AddDrain(name string, fn func(ctx context.Context)) {
	s.mu.Lock()
	s.drains = append(s.drains, namedDrain{name, fn})
	s.mu.Unlock()
}

// AddReload 注册 SIGHUP 回调。
func (s *Supervisor) AddReload(fn func()) {
	s.mu.Lock()
	s.reloads = append(s.reloads, fn)
	s.mu.Unlock()
}

// Drain 按注册序排空全部组件（≤ DrainTimeout）。
func (s *Supervisor) Drain(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, DrainTimeout)
	defer cancel()
	s.mu.Lock()
	ds := append([]namedDrain(nil), s.drains...)
	s.mu.Unlock()
	for _, d := range ds {
		done := make(chan struct{})
		go func() { d.fn(cctx); close(done) }()
		select {
		case <-done:
		case <-cctx.Done():
			// 超时：继续排下一个（crash-only：状态交给重启恢复）。
		}
	}
}

// Run 监听信号直至 Quit。返回 ExitOK 或 ExitInvariant。
// signals 参数生产传 nil（内部 signal.Notify），测试传 chan。
func (s *Supervisor) Run(ctx context.Context, signals <-chan os.Signal) (ExitCode, error) {
	if signals == nil {
		ch := make(chan os.Signal, 8)
		signal.Notify(ch)
		defer signal.Stop(ch)
		signals = ch
	}
	ec := make(chan ExitCode, 1)
	s.set.Watch(ctx, signals, func(ev signalset.SignalEvent) {
		switch ev.Semantics {
		case signalset.Reload:
			s.fireReload()
		case signalset.Snapshot:
			s.firePanic() // SIGUSR1 状态 dump 复用快照回调
		case signalset.Quit:
			s.fireShutdown()
			s.Drain(context.Background())
			select {
			case ec <- ExitOK:
			default:
			}
		}
	})
	select {
	case <-ctx.Done():
		return ExitOK, ctx.Err()
	case code := <-ec:
		return code, nil
	}
}

func (s *Supervisor) fireReload() {
	s.mu.Lock()
	fns := append([]func(){}, s.reloads...)
	s.mu.Unlock()
	for _, fn := range fns {
		safeCall(fn)
	}
	if s.OnReload != nil {
		safeCall(s.OnReload)
	}
}

func (s *Supervisor) fireShutdown() {
	if s.OnShutdown != nil {
		safeCall(s.OnShutdown)
	}
}

func (s *Supervisor) firePanic() {
	s.mu.Lock()
	fns := append([]func(){}, s.panics...)
	s.mu.Unlock()
	for _, fn := range fns {
		safeCall(fn)
	}
	if s.OnPanic != nil {
		safeCall(s.OnPanic)
	}
}

// AddSnapshot 注册 SIGUSR1 状态 dump 回调。
func (s *Supervisor) AddSnapshot(fn func()) {
	s.mu.Lock()
	s.panics = append(s.panics, fn)
	s.mu.Unlock()
}

func safeCall(fn func()) {
	defer func() { _ = recover() }() // 回调故障不杀 supervisor
	fn()
}
