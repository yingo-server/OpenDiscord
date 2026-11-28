# 后端补充端点 · 契约草案（供后端实现）

> 背景：UFOT 后端用户面当前**没有**“列出我的会话 / 搜索用户”的只读端点，
> 客户端只能靠 WS 事件（kind 202 CONV_PAYLOAD）被动累积会话，无法新建会话、
> 无法做全量搜索。本文件给出两个最小只读端点的契约草案，前端已按此实现
> （端点未上线时自动降级为“事件累积 + 手动输入 ID”）。
>
> 约定沿用用户面既有风格：路径 /api/*，鉴权 access_token（Cookie 或 Bearer），
> 成功体 JSON，错误体 {error, code}（与既有端点一致），列表体 {items, total}。

## 1. GET /api/conversations

列出当前用户参与的会话。

**请求**

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| limit | int | 50 | 1..200 |
| offset | int | 0 | ≥0 |
| q | string | — | 标题 / 成员昵称模糊匹配，可选 |

**响应 200**

```json
{
  "items": [
    {
      "convId": 42,
      "title": "项目讨论",
      "convType": 1,
      "createdAt": 1795533858,
      "memberCount": 5,
      "members": [
        { "userId": 12, "role": 0, "joinedAt": 1795533858, "mutedUntil": 0 }
      ],
      "lastSeq": 128,
      "unread": 3,
      "lastMessage": {
        "msgId": 9001,
        "seq": 128,
        "senderId": 12,
        "sentAt": 1795533858,
        "text": "今天下午三点开会"
      }
    }
  ],
  "total": 1
}
```

字段与既有 WS 载荷一致（`ConvPayload` / `MsgNew` 的 camelCase 命名），
便于客户端复用同一套解析逻辑。

**实现约束**

- 只返回调用者**是成员**的会话；非成员不得出现在列表中。
- `unread` 是服务端估算值（提示性质，不得作为鉴权依据）。
- 大群可省略 `members` 成员明细并给出 `memberCount`（对齐 CONV_PAYLOAD 的
  `membersOmitted` 语义）。
- 数据来源：热库 conv_conversations / conv_members / msg_messages_hot；
  冷库合并沿用 readrouter 的 hot/cold 路由。

## 2. GET /api/users

搜索用户（用于新建会话时的成员选择）。

**请求**

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| q | string | — | 昵称 / 用户 ID 前缀模糊匹配 |
| limit | int | 30 | 1..100 |
| offset | int | 0 | ≥0 |

**响应 200**

```json
{
  "items": [
    {
      "user_id": 12,
      "display_name": "林可",
      "avatar_media_id": "",
      "status": 1,
      "created_at": 1790000000
    }
  ],
  "total": 1
}
```

**实现约束**

- 只暴露公开资料字段，**不得**返回手机号、邮箱、凭据、设备信息。
- 被封禁用户（status != 1）不出现在结果里。
- 搜索要走现有索引（identity_users.display_name），禁止全表扫描。

## 3. 与既有能力的关系

| 能力 | 现状 | 补充后 |
|---|---|---|
| 读会话列表 | 仅 kind 202 事件累积 | 事件 + 主动拉取，可恢复、可搜索 |
| 新建会话 | 需先知道对方 user_id | 可搜索用户后建群/单聊 |
| 改昵称 | kind 940 不在用户层能力表，客户端不可达 | 仍不可达（需单独开权限，本次不含） |
| 消息搜索 | 仅管理面 REST | 用户面仍无（本次不含） |

## 4. 客户端降级行为（已实现）

- `GET /api/conversations` 返回 404 → 客户端不报错，保留事件累积列表，
  并在联系人页提示“后端尚未开放用户检索接口”，允许手动输入用户 ID 建会话。
- `GET /api/users` 返回 404 → 联系人页展示手动输入 ID 的兜底表单。
