package pipe

import (
	"context"

	"github.com/yingo-server/OpenDiscord/server/internal/proto"
)

// emit.go: test-shell injection of Req frames with full routing fields.
// The production FrameSource reads chatpb fields; tests use these helpers to
// feed the dispatcher directly (bypassing the shell-only default source).

// dispatchReq feeds one Req straight to the dispatcher (test seam; the
// production path goes through Run/FrameSource).
func (p *Pipe) dispatchReq(ctx context.Context, r Req) { p.dispatch(ctx, r) }

// FeedReqForTest 把一个已经由装配层 FrameSource 转换好的 Req 交给
// dispatcher（chatd 装配层测试用；避免测试重复实现 FrameSource 的转换）。
func (p *Pipe) FeedReqForTest(ctx context.Context, r Req) { p.dispatch(ctx, r) }

// FeedFrameForTest 把一个内部帧经装配层安装的 FrameSource 转换后喂给
// dispatcher（chatd 装配层测试用；生产路径由 Run 的 peer.Frames() 驱动）。
func (p *Pipe) FeedFrameForTest(ctx context.Context, f *proto.Frame) {
	if f == nil {
		return
	}
	p.dispatch(ctx, p.FrameSource(f))
}
