package pipe

import (
	"sync"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

// 扇出路径去全局锁（perf-spec 补充模块 B）后的并发安全压测：
// 多 goroutine 直接并发调 sendOne（绕过 lane），等价 fanSender + fanout
// 投递路径 + reply sender 同时发送。CI -race 下零 race、零丢帧。

// drainOut pumps tp.Out() until the peer is closed, then drains whatever is
// still buffered and exits. TestPeer.Out() is never closed by anyone (Close()
// only closes the `closed` channel), so a bare `for range tp.Out()` can never
// terminate — the drain goroutine must key its exit off tp.closed instead.
func drainOut(tp *TestPeer, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-tp.Out():
		case <-tp.closed:
			// peer closed: no more Send can succeed; flush the buffer.
			for {
				select {
				case <-tp.Out():
				default:
					return
				}
			}
		}
	}
}

// TestSendOneConcurrentNoGlobalLock: 8 goroutine × 250 帧并发 sendOne，
// TestPeer 的 out channel（cap 64）会满 —— sendOne 不阻塞（扇出允许丢），
// reply 路径走 replySendMu。断言不 panic、无 race、全部调用在有限时间内返回。
func TestSendOneConcurrentNoGlobalLock(t *testing.T) {
	p := &Pipe{peer: NewTestPeer(), lg: log.For("pipe-test")}
	p.SetEmit(nil) // DefaultEmit: 测试只关心 sendOne 的锁路径，帧内容无关
	// NewTestPeer 的 Send 是阻塞 channel send（cap 64）；并发压测必须持续
	// 排水，否则 8×250 帧塞满后所有 goroutine 永久阻塞在 Send 上。
	tp := p.peer.(*TestPeer)
	done := make(chan struct{})
	go drainOut(tp, done)
	var wg sync.WaitGroup
	const goroutines = 8
	const per = 250
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(isReply bool) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				p.sendOne(&env{Kind: 101, Payload: []byte("x"), isReply: isReply})
			}
		}(g%2 == 0)
	}
	wg.Wait()
	// 关闭 peer 让排水 goroutine 退出（Send 之后只会返回 errPeerClosed）。
	_ = p.peer.Close()
	<-done
	// 无 panic 即通过；TestPeer 满载丢弃由 select/default 语义兜住。
}

// TestSendOneReplyAndFanoutInterleave: reply 与 fanout 并发发送。断言目标：
// 并发下 sendOne 不需要全局锁（扇出与 reply 交错）、全部 sendOne 调用在
// 有限时间内返回、无 panic、排水 goroutine 正常退出。ACK 送达率在单消费者
// 竞争下结构上不可断言（谁消费到帧取决于调度），由 e2e 覆盖。
func TestSendOneReplyAndFanoutInterleave(t *testing.T) {
	p := &Pipe{peer: NewTestPeer(), lg: log.For("pipe-test")}
	p.SetEmit(nil) // DefaultEmit: 测试只关心 sendOne 的锁路径，帧内容无关
	tp := p.peer.(*TestPeer)
	done := make(chan struct{})
	go drainOut(tp, done)
	const n = 200
	var wg sync.WaitGroup
	// 扇出：与 reply 交错竞争同一个 peer 的 out 通道（cap 64，会被塞满）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n*4; i++ {
			p.sendOne(&env{Kind: 201, Payload: []byte("fan"), isReply: false})
		}
	}()
	// reply：走 replySendMu 的路径
	for i := 0; i < n; i++ {
		p.sendOne(&env{Kind: 102, Payload: []byte("ack"), isReply: true})
	}
	wg.Wait()
	// 关闭 peer 让排水 goroutine 退出；本测试验证的是「reply 与扇出并发时
	// 无死锁、无 panic」（锁分片的并发安全），ACK 送达率由 e2e 覆盖。
	_ = p.peer.Close()
	<-done
}
