// Package featureflags —— v3 总线灰度判定（docs/v3-bus-design.md §6/§7.3 T-SV-7）。
//
// 规则：同 uid 稳定落同一路径——按 uid 哈希（FNV-1a 32 位）取模百分比分桶，
// 与连接无关、与重启无关。flag 语义：
//   - v3_bus_enabled = false → 一律旧路径（整体开关优先，回滚主路径）；
//   - v3_bus_percent = 0..100 → uid_hash % 100 < percent 走 v3。
//
// 该文件是纯函数（机制），无状态；读 flag 的策略在 P2 bus 层调用点完成。
package featureflags

import "hash/fnv"

// InV3Bucket 判定 uid 是否落在 v3 灰度桶内。
// flags 传 Snapshot() 副本即可；enabled=false 或 percent<=0 时恒为 false。
// percent>=100 时全量（含 enabled=true 前提）。
func InV3Bucket(flags map[string]any, uid uint64) bool {
	if !BoolMap(flags, KeyV3BusEnabled) {
		return false
	}
	percent, ok := ToInt(flags[KeyV3BusPercent])
	if !ok || percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	h := fnv.New32a()
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(uid >> (8 * (7 - i)))
	}
	_, _ = h.Write(buf[:])
	return int(h.Sum32()%100) < percent
}

// BoolMap 从 flag map 读 bool（快照副本上的便捷读取，与 Flags.Bool 同语义）。
func BoolMap(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		if b, isB := v.(bool); isB {
			return b
		}
		// JSON/DB 装载路径：字符串 "true"/"1" 亦接受（与 911 下发形态一致）
		if s, isS := v.(string); isS {
			return s == "true" || s == "1"
		}
		if n, ok2 := ToInt(v); ok2 {
			return n != 0
		}
	}
	return false
}
