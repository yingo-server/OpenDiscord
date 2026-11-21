# 术语字典（界面 / 文档 / API 协议 / CLI 共同基线）

本字典是 OpenDiscord 的用词真相源：一个概念只用一个名称。新增用户可见名词前先更新本表。
来源：sota-ux-writing "Name things once" 纪律（v1.5.0-beta.1）。

| 术语 | 界面用词 | 文档用词 | API / 协议字段名 | 定义 |
|---|---|---|---|---|
| Conversation | 会话 | 会话 | `conv_id` / `conversation` | 一条单聊或群聊的消息容器。 |
| Direct conversation | 单聊 | 单聊 | `conv_type=direct` | 两名用户之间的会话。 |
| Group conversation | 群聊 | 群聊 | `conv_type=group` | 多名成员参与的会话。 |
| Message | 消息 | 消息 | `msg_id` / `message` | 会话中发送的一条内容记录。 |
| Message record | 消息记录 | 消息记录 | `MessageRecord` | 管理端检索到的消息数据结构。 |
| Draft | 草稿 | 草稿 | `draft` / `DRAFT` | 尚未发送的文本内容。 |
| Sending | 发送中 | 发送中 | `sending` / pending outbox | 客户端已提交但服务端尚未确认的消息状态。 |
| Sent | 已发送 | 已发送 | `MSG_ACK` / `sent` | 服务端已接收消息。 |
| Delivered | 送达 | 送达 | `MSG_DELIVERED` / `delivered` | 消息已投递到目标客户端。 |
| Read | 已读 | 已读 | `MSG_READ` / `read` | 接收方已查看消息。 |
| Unread message | 未读消息 | 未读 | `unread_count` | 当前用户尚未查看的消息。 |
| Receipt | 回执 | 回执 | `MSG_ACK` / `MSG_READ` / `MSG_DELIVERED` | 服务端或接收端对消息状态的确认。 |
| Watermark | 水位线 | 水位线 | `watermark` / `WATERMARK`(14) | 拉取模式下标记同步进度的位置。 |
| Sequence number | 会话序号 | 序号 | `seq` | 会话内单调递增的消息序号。 |
| Sync request | 同步请求 | 补拉请求 | `SYNC_REQUEST`(8) / `sinceSeq` | 客户端发现缺口后请求补齐消息。 |
| Recall | 撤回 | 撤回 | `RECALL`(107) / `recall` | 让消息在所有端显示撤回提示。 |
| Delete message | 删除消息 | 删除 | `DELETE`(108) / `delete` | 从本地或服务端移除消息记录。 |
| Report | 举报 | 举报 | `REPORT`(453) / `report_id` | 用户对消息、用户或会话提交的违规反馈。 |
| Reporter | 举报人 | 举报人 | `reporter_id` | 提交举报的用户。 |
| Report handling | 举报处置 | 举报处置 | `report.handle` / `action` / `note` | 管理员认领、处理或驳回举报。 |
| Evidence | 证据 | 举报证据 | `evidence[]` | 举报相关的加密保全材料。 |
| Ban | 封禁 | 封禁 | `ban` / `level` / `until` | 撤销账号登录凭证并断开连接。 |
| Unban | 解封 | 解封 | `unban` | 恢复被封禁账号的登录能力。 |
| Kick | 踢出 | 强制下线 | `kick` | 断开账号当前全部连接，不撤销凭证。 |
| User | 用户 | 用户 | `user_id` / `uid` | 使用 OpenDiscord 的账号。 |
| Feature flag | 功能开关 | 特性开关 | `flags` / `key` / `value` / `FEATURE_FLAGS`(911) | 可即时下发的策略开关。 |
| Capability negotiation | 能力协商 | 能力协商 | `CLIENT_CAPS`(18) | 客户端上报支持能力以确定可用协议分支。 |
| Pull capability | 拉取能力 | 普遍拉取能力 | `pull_capable` | 客户端可使用水位线进行补拉的能力。 |
| Audit log | 审计日志 | 审计日志 | `audit_log` / `audit` | 记录管理和高危操作的只追加日志。 |
| Audit chain | 审计链 | 哈希链审计 | `audit_chain` / `audit_chain_broken` | 审计记录的防篡改链式校验。 |
| Archive | 归档 | 归档 | `archive` | 将热库旧数据搬运到冷库。 |
| Archive run | 归档运行 | 归档运行记录 | `archive_runs` | 一次归档作业的执行账本。 |
| Hot database | 热库 | 热库 | `hot.db` / `OPENDISCORD_HOT_DB` | 本机 SQLite，保存近期数据。 |
| Cold database | 冷库 | 冷库 | `cold_dsn` | 外部 PostgreSQL，保存归档数据。 |
| Push subscription | 推送订阅 | 推送订阅 | `POST /api/push/subscribe` / `subscription` | 浏览器 Web Push 的订阅登记。 |
| Web Push notification | 系统通知 | 系统通知 | `Notify` / `WATERMARK` 触发 | 发往设备的操作系统级提醒。 |
| Capacity contract | 容量契约 | 容量契约 | `policy.capacity` / `docs/capacity-contract.md` | 服务端承诺的可用容量上限与超限行为约定。 |
| Admission control | 准入限流 | 准入 | `policy.capacity.admission_qps` / `admission_qps` | 对写操作（MSG_SEND）做全局令牌准入，超限即拒绝。 |
| Rate limit | 限流 | 限流 | `RATE_LIMITED`(13) / `retry_after_ms` | 请求频率超过上限，服务端显式拒绝而非静默丢弃。 |
| Explicit rejection | 显式拒绝 | 显式拒绝 | `RATE_LIMITED`(13) / `ERROR`(10) | 超限时立即告知用户“未受理/未发送”，不挂起等待。 |
| Delivery failure | 送达失败 | 送达失败 | `MSG_ACK` `status != 0` | 服务端未接受该消息，客户端把该条消息标记为发送失败。 |
| Send failure | 发送失败 | 发送失败 | outbox `status=failed` | 消息未离开本机发送队列；红点可重试。 |
| Server rejection | 服务端拒绝 | 服务端拒绝 | `ERROR`(10) `reason` | 服务端对本次请求给出错误原因，原因码仅用于工单追溯。 |
