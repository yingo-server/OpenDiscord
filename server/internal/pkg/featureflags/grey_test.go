package featureflags

import "testing"

// TestV3BusDefaults 锁定 1.4 转生产默认：v3 总线全量开启（S-1~S-7 压测守门）。
// 回滚路径由显式 NewFromMap(off) 构造，见 TestInV3BucketKillSwitch。
func TestV3BusDefaults(t *testing.T) {
	f := New()
	if !f.Bool(KeyV3BusEnabled, false) {
		t.Fatal("v3_bus_enabled default must be true after 1.4 production rollout")
	}
	if got := f.Int(KeyV3BusPercent, -1); got != 100 {
		t.Fatalf("v3_bus_percent default = %d, want 100", got)
	}
}

// TestInV3BucketStable 锁定 T-SV-7：同 uid 稳定落同一路径（按 uid 哈希，非按连接随机）。
func TestInV3BucketStable(t *testing.T) {
	flags := map[string]any{KeyV3BusEnabled: true, KeyV3BusPercent: 50}
	first := InV3Bucket(flags, 12345)
	for i := 0; i < 1000; i++ {
		if InV3Bucket(flags, 12345) != first {
			t.Fatal("same uid must always land in the same path (T-SV-7)")
		}
	}
	// flag 无关：不同 map 副本同结果
	if InV3Bucket(SnapshotCopy(flags), 12345) != first {
		t.Fatal("snapshot copy must not change bucket decision")
	}
}

// TestInV3BucketKillSwitch 锁定整体开关优先：enabled=false 时无论 percent 一律旧路径。
func TestInV3BucketKillSwitch(t *testing.T) {
	off := map[string]any{KeyV3BusEnabled: false, KeyV3BusPercent: 100}
	if InV3Bucket(off, 1) {
		t.Fatal("enabled=false must force legacy path even at 100%")
	}
}

// TestInV3BucketDistribution 锁定百分比语义：100% 全量、0% 全关。
func TestInV3BucketDistribution(t *testing.T) {
	full := map[string]any{KeyV3BusEnabled: true, KeyV3BusPercent: 100}
	for uid := uint64(1); uid <= 500; uid++ {
		if !InV3Bucket(full, uid) {
			t.Fatalf("uid %d excluded at 100%%", uid)
		}
	}
	zero := map[string]any{KeyV3BusEnabled: true, KeyV3BusPercent: 0}
	for uid := uint64(1); uid <= 500; uid++ {
		if InV3Bucket(zero, uid) {
			t.Fatalf("uid %d included at 0%%", uid)
		}
	}
	// 粗分布校验：5% 灰度下 5000 个 uid 命中数应在 150-350 区间（期望 250）
	five := map[string]any{KeyV3BusEnabled: true, KeyV3BusPercent: 5}
	hits := 0
	for uid := uint64(1); uid <= 5000; uid++ {
		if InV3Bucket(five, uid) {
			hits++
		}
	}
	if hits < 150 || hits > 350 {
		t.Fatalf("5%% bucket hits = %d, want ~250 (150-350)", hits)
	}
}

// SnapshotCopy 测试辅助：构造等价 map 副本。
func SnapshotCopy(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}
