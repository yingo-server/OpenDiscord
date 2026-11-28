// Package verifier 是流水线第 2 级：转发许可校验
// （docs/bridge-design.md §2.2）。检查：入口层名已注册、
// kind 在该层能力集内。非法帧 → Reject chan + 丢帧。
//
// 禁令：不路由、不做用户权限、不解析 payload、不做密码学。
package verifier

import (
	"context"

	"github.com/yingo-server/UFOT/server/internal/proto"
)

// Reject code 常量（连接级降级，proto.Reject.Code）。
const (
	CodeUnknownLayer uint32 = 1 // NO_HELO / 未注册层
	CodeNoCapability uint32 = 2 // kind 超出层能力集
)

// Verifier 校验级。能力表 map[layer]set(kinds) 由配置注入（启动时静态）。
type Verifier struct {
	In      chan *proto.Frame
	Out     chan *proto.Frame
	Rejects chan *proto.Reject

	caps map[string]map[uint32]bool // layer → kind 集合
}

// New 构造。caps 形如 {"user": []kinds.Kind{...}}，内部转 set。
func New(in, out chan *proto.Frame, rejects chan *proto.Reject, caps map[string][]uint32) *Verifier {
	v := &Verifier{In: in, Out: out, Rejects: rejects, caps: map[string]map[uint32]bool{}}
	for layer, ks := range caps {
		set := map[uint32]bool{}
		for _, k := range ks {
			set[k] = true
		}
		v.caps[layer] = set
	}
	return v
}

// Run 消费 In，合法帧投 Out；非法帧投 Rejects 并丢弃。
// 层未注册或能力外均只丢帧（断连由 Session/Egress 依据 Reject 决定）。
func (v *Verifier) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case f, ok := <-v.In:
			if !ok {
				return nil
			}
			if r, bad := v.check(f); bad {
				if v.Rejects != nil {
					select {
					case v.Rejects <- r:
					case <-ctx.Done():
						return ctx.Err()
					default: // Rejects 满：降级日志路径，不阻塞主流水线
					}
				}
				continue
			}
			select {
			case v.Out <- f:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// check 返回 (reject, 是否拒绝)。纯函数。
func (v *Verifier) check(f *proto.Frame) (*proto.Reject, bool) {
	layer := ""
	if f.Meta != nil {
		layer = f.Meta.SrcLayer
	}
	set, ok := v.caps[layer]
	if !ok {
		return &proto.Reject{Kind: f.Kind, Code: CodeUnknownLayer, Reason: "source layer not registered: " + layer}, true
	}
	if !set[f.Kind] {
		return &proto.Reject{Kind: f.Kind, Code: CodeNoCapability, Reason: "kind not in capability set of layer " + layer}, true
	}
	return nil, false
}
