// capability_test.go —— 旧客户端能力协商的连接注册表测试
// （lts-1.4-acceptance §B.5「能力表读写」）。
//
// 覆盖：
//   - 缺省无能力（没上报 = 老客户端 = inline）；
//   - 读写 / 覆盖（同一连接重复上报只保留最后一次）；
//   - uid 粒度聚合：全部连接有能力才算有能力；
//   - 连接注销后能力条目消失（connID 回绕不得继承旧能力）。
package session

import "testing"

type capConn struct {
	id  uint64
	uid int64
}

func (c *capConn) ID() uint64          { return c.id }
func (c *capConn) UserID() int64       { return c.uid }
func (c *capConn) DeviceID() string    { return "d" }
func (c *capConn) Write(b []byte) bool { return true }
func (c *capConn) Close() error        { return nil }

func TestCapabilityTable(t *testing.T) {
	s := New(0)

	// 缺省：任何连接都没有能力。
	c1 := &capConn{id: 1, uid: 100}
	if err := s.Register(c1); err != nil {
		t.Fatalf("register: %v", err)
	}
	if s.PullCapableConn(1) {
		t.Fatalf("a fresh connection must not be pull-capable by default")
	}
	if s.PullCapableUID(100) {
		t.Fatalf("a uid without any capability report must not be pull-capable")
	}

	// 写入能力。
	s.SetPullCapable(1, true)
	if !s.PullCapableConn(1) {
		t.Fatalf("capability write lost")
	}
	if !s.PullCapableUID(100) {
		t.Fatalf("single capable connection must make the uid pull-capable")
	}

	// 同 uid 的第二条连接没上报 → 整个 uid 保守地按老客户端处理。
	c2 := &capConn{id: 2, uid: 100}
	if err := s.Register(c2); err != nil {
		t.Fatalf("register c2: %v", err)
	}
	if s.PullCapableUID(100) {
		t.Fatalf("uid with one old client must NOT be pull-capable (no message loss allowed)")
	}
	// 第二条连接也上报 → 恢复为有能力。
	s.SetPullCapable(2, true)
	if !s.PullCapableUID(100) {
		t.Fatalf("all connections capable → uid must be pull-capable")
	}

	// 覆盖：最后一次上报为准。
	s.SetPullCapable(2, false)
	if s.PullCapableUID(100) {
		t.Fatalf("a revoked capability must take effect")
	}
	s.SetPullCapable(2, true)

	// 断连清理：能力条目随连接一起消失。
	s.Unregister(2)
	if s.PullCapableConn(2) {
		t.Fatalf("capability of an unregistered connection must be cleared")
	}
	if !s.PullCapableUID(100) {
		t.Fatalf("remaining capable connection must keep the uid pull-capable")
	}

	// 没有在线连接的 uid = 没有收件人 → false。
	if s.PullCapableUID(999) {
		t.Fatalf("offline uid must not be pull-capable")
	}
}
