# 显式容量契约（V2-0）

状态：v2.0 引入 · 开关：`capacity_admission_enabled`（日志）+ `policy.capacity.admission_qps`（准入）

## 为什么需要

v1.4/v1.5 的压测显示：raw ceiling（无节流）下 500 人群×1000 在线可用性 0.93%、
1000×50 为 16.38%——单进程 SQLite 写路径的物理天花板。此前超限时 `msg_send`
要么静默超时（客户端 outbox 挂 10s），要么收到不配对的 RATE_LIMITED——用户看到
"消息一直转圈"，不知道被拒。

V2-0 的诚实做法：**给出明确的容量轴，超限时立即、明确地拒绝**，而不是假装接受再挂起。

## 配置

`config/policy.json` 顶层新增（0 / 缺省 = 禁用，逐字节等价旧行为）：

```json
{
  "capacity": {
    "max_concurrent_conns": 0,
    "admission_qps": 0
  }
}
```

- `admission_qps`：全局准入 QPS 上限（对 `MSG_SEND` 写操作消耗令牌；心跳/豁免帧不消耗）。
  桶参数 `{Rate,Burst,Capacity}=qps`，即最多突发 1 秒的量。超限返回 Throttle。
- `max_concurrent_conns`：保留字段（连接数准入轴），当前版本**预留未接线**——
  连接数上限由部署层容量规划负责；本版先落地 QPS 轴。负数被 `policy.Validate` 拒绝。

SIGHUP 热改：复用 `usergw ReloadPolicy` → `limiter.SetConfig` 路径，改 `capacity.admission_qps`
下一次发送即生效，无需重启。

## 行为

- `capacity.admission_qps=0`（默认）：准入完全不执行，与 v1.5 行为逐字节一致。
- `>0`：每帧 `MSG_SEND` 先消耗一个全局令牌；超限 → kind13 `RATE_LIMITED`，
  其 `AckId` = 触发帧的 `Envelope.Id`（现成字段，零协议改动）。
- 客户端（v2.0 起）：收到 RATE_LIMITED / ERROR 时把 in-flight 消息**立即标记失败**
  （红点），不再等 10s ACK 超时；MSG_ACK `status!=0` 同样转失败，"ACK 不撒谎"。
- `capacity_admission_enabled` flag（默认 false）：只控制服务端 `admission.reject`
  结构化日志（可观测性），**不改变准入是否生效**——准入由 `admission_qps>0` 决定。

## 与压测门槛的关系

V2-0 是契约化/正确性改动，不是吞吐优化：它让"超限"从挂起变明确，但**不提升 S-1/S-2 的
可用性数字**。真正的写入吞吐提升在 V2-1（写入批量化，见 docs/v2.1-write-batching-spec.md）。
容量门槛与 raw ceiling 测试条件的不匹配仍如实记录在 docs/lts-1.4-plan.md §9。

## 可观测

flag-on 且准入超限时服务端日志：
`WARN admission.reject axis=admission_qps kind=MSG_SEND uid=… conn=… env_id=…`
