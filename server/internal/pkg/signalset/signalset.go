// Package signalset 统一管理进程信号语义（docs/bridge-design.md §5）：
// SIGHUP=Reload、SIGTERM/SIGINT=Quit、SIGUSR1=Snapshot、SIGUSR2=Custom。
// 全仓库除本包与 bridge/supervisor 外禁止 import os/signal（CI import-lint）。
package signalset

import (
	"context"
	"os"
	"os/signal"
)

// Semantics 信号语义类别。
type Semantics int

const (
	// Reload 配置热重载（SIGHUP）。
	Reload Semantics = iota
	// Snapshot 状态 dump（SIGUSR1）。
	Snapshot
	// Quit 优雅退出（SIGTERM/SIGINT）。
	Quit
	// Custom 预留语义（SIGUSR2：GC/heap dump 等）。
	Custom
)

// SignalEvent 一次信号事件：语义 + 原始信号编号。
type SignalEvent struct {
	Semantics Semantics
	Signal    os.Signal
}

// Set 信号语义注册表。零值可用；信号源可通过 watch 参数注入以便测试。
type Set struct {
	// mapping 信号 → 语义，默认已注册系统信号；可覆盖。
	mapping map[os.Signal]Semantics
}

// DefaultMapping is defined per platform: Windows has no SIGHUP/SIGUSR1/2, so
// the unix table cannot compile there. See signalset_unix.go / _windows.go.

// NewSet 用默认映射构造。
func NewSet() *Set {
	m := DefaultMapping()
	return &Set{mapping: m}
}

// Register 注册/覆盖某信号的语义。
func (s *Set) Register(sig os.Signal, sem Semantics) {
	if s.mapping == nil {
		s.mapping = map[os.Signal]Semantics{}
	}
	s.mapping[sig] = sem
}

// Map 返回当前映射的只读副本。
func (s *Set) Map() map[os.Signal]Semantics {
	m := make(map[os.Signal]Semantics, len(s.mapping))
	for k, v := range s.mapping {
		m[k] = v
	}
	return m
}

// Watch 开始监听并按映射翻译为事件回调。signals 参数为注入的信号源：
// 生产传 signal.Notify 的 channel，测试传任意 chan os.Signal。
// ctx 结束即停止监听。重复语义不会去重——每个信号一次回调。
func (s *Set) Watch(ctx context.Context, signals <-chan os.Signal, handler func(SignalEvent)) {
	go func() {
		defer func() { _ = recover() }() // 测试中 handler 可能随 ctx 一起被 GC
		for {
			select {
			case <-ctx.Done():
				return
			case sig, ok := <-signals:
				if !ok {
					return
				}
				sem, known := s.mapping[sig]
				if !known {
					continue
				}
				if handler != nil {
					handler(SignalEvent{Semantics: sem, Signal: sig})
				}
			}
		}
	}()
}

// NotifyAll 用默认信号集创建系统信号源（os/signal 只允许出现在本包）。
// 返回的 channel 停止方式：ctx 结束后调用 stop。
func NotifyAll(ctx context.Context) (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 8)
	sigMap := DefaultMapping()
	sigs := make([]os.Signal, 0, len(sigMap))
	for k := range sigMap {
		sigs = append(sigs, k)
	}
	watchSignal(ch, sigs...)
	stop := func() { stopSignal(ch) }
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ch, stop
}

// ---- os/signal 桥接（全仓库仅本包与 bridge/supervisor 允许 import os/signal）----

// watchSignal 包装 signal.Notify。
func watchSignal(ch chan<- os.Signal, sigs ...os.Signal) {
	signal.Notify(ch, sigs...)
}

// stopSignal 包装 signal.Stop。
func stopSignal(ch chan<- os.Signal) {
	signal.Stop(ch)
}
