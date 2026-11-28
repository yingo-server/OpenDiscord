# Web Push（系统推送）部署与验证手册

适用范围：服务端推送订阅端点（`POST /api/push/subscribe`）+ VAPID 发送。

> 交付边界：服务端**订阅端点可用**、**Notify API 可调用**
> （`pushsvc.Service.Notify(uid, payload)`）、以及一个**测试触发入口**
> （`POST /api/push/test`）。尚未接入"每条消息送达即推送"的投递路径，
> 因此现在手机仍不会收到每条消息的实时系统通知，只在手动触发测试推送时收到。

---

## 0. 客户端需要做的三件事

1. **申请订阅**：在用户授权通知后，用 Push API（`serviceWorkerRegistration.pushManager.subscribe({userVisibleOnly:true, applicationServerKey})`）取得 PushSubscription（endpoint + keys.p256dh + keys.auth）。VAPID 公钥由部署方分发（生成方式见 §1）。
2. **把订阅 POST 到服务端**：`POST /api/push/subscribe`，body
   `{"subscription":{"endpoint":"https://...","keys":{"p256dh":"<base64url 65B>","auth":"<base64url 16B>"}}}`
   （带登录 Cookie）→ 成功 **204 无 body**。同 endpoint 重复上报幂等。
3. **fail-closed 处理失败**：任何非 2xx 只记日志、不重试风暴；推送不可用不得影响
   WS 长连接与页内通知。

字段级错误语义见 [client-contract.md §5](client-contract.md) 与 [api-reference.md §4a](api-reference.md)。

## 1. 生成 VAPID 密钥对

VAPID 密钥是一对 P-256（ECDSA）密钥：公钥给客户端（`applicationServerKey`），
私钥只留服务端。用 OpenSSL 生成（任选其一）：

```bash
# 方式 A：PEM（SEC1）私钥 + 原始公钥
openssl ecparam -genkey -name prime256v1 -noout -out vapid.pem
openssl ec -in vapid.pem -pubout -out vapid_pub.pem

# 方式 B：直接从 PEM 导出 base64url 公钥（分发给客户端用）
openssl ec -in vapid.pem -pubout -outform DER 2>/dev/null | tail -c 65 | base64 | tr '+/' '-_' | tr -d '=\n'
```

上面 `tail -c 65` 取的是未压缩点（`0x04 || X || Y`），
再用 base64url（去掉 padding）编码。

推荐：用 `web-push` 的官方工具（最省心）：

```bash
npx web-push generate-vapid-keys
# → { publicKey: "...", privateKey: "..." }
```

## 3. 服务端注入私钥与联系人

服务端从**进程环境变量**读取（`server/internal/pushsvc/sender.go`）：

| 变量 | 必填 | 说明 |
| --- | --- | --- |
| `VAPID_PRIVATE_KEY` | 是 | VAPID 私钥。支持三种形态：① PEM（`-----BEGIN EC PRIVATE KEY-----` / `-----BEGIN PRIVATE KEY-----`）；② base64(PEM)；③ baseurl 编码的 32 字节 P-256 原始标量 |
| `VAPID_SUBJECT` | 是 | 推送服务联系地址，必须 `mailto:` 或 `https://` 前缀，例如 `mailto:admin@example.com` |

示例（docker/systemd 环境）：

```bash
export VAPID_PRIVATE_KEY="$(cat /etc/ufot/vapid.pem)"
export VAPID_SUBJECT="mailto:admin@example.com"
```

**缺失或非法时的行为（fail-closed）**：启动日志出现
`push disabled: VAPID_PRIVATE_KEY/VAPID_SUBJECT not configured`（WARN），
Sender 退化为 no-op —— 订阅端点**照常可用**（204），但不会真正投递任何通知。
这是刻意设计：坏配置不能让服务器起不来，也不该让客户端误以为"已订阅成功但收不到"
之外的行为不一致。

## 4. 验证订阅端点

前置：客户端已登录（拿到 `access_token` Cookie）。

### 4.1 手工验证

```bash
# 取 access_token（登录后从浏览器 Cookie 复制）
curl -i -X POST https://<host>/api/push/subscribe \
  -H 'Content-Type: application/json' \
  -b "access_token=<TOKEN>" \
  -d '{"subscription":{"endpoint":"https://fcm.googleapis.com/fcm/send/xxxx","keys":{"p256dh":"<b64url 65B>","auth":"<b64url 16B>"}}}'
```

预期：`HTTP/1.1 204 No Content`。其他状态：

| 状态 | 含义 |
| --- | --- |
| 401 | 缺少/无效 `access_token` Cookie |
| 400 | body 不是 JSON，或缺 `endpoint` / `keys.p256dh` / `keys.auth`，或 endpoint 非 https、或密钥长度非法 |
| 500 | 订阅存储失败（内存态，实际极少出现） |

客户端对任何非 2xx 都 fail-closed：只记日志、不弹错、不重试风暴。

### 4.2 端到端（真机）

1. 客户端拿到 VAPID 公钥并完成订阅上报；站点需 HTTPS（Push API 与 ServiceWorker
   强制安全上下文）。
2. 手机浏览器打开客户端并允许通知权限。
3. 服务端日志出现 `push subscription registered`（uid 已脱敏记录）。
4. 触发测试推送（测试入口，仅推给调用者自己）：

```bash
curl -i -X POST https://<host>/api/push/test -b "access_token=<TOKEN>"
# → {"sent":1}
```

5. 手机应收到系统通知。若 `sent:0` 说明该账号没有存活订阅（重新打开网页触发
   订阅登记）；若有 `sent` 但收不到，检查 `push send failed` 日志与推送服务返回码。

## 5. 运维要点

- **订阅是内存态**（`map[uid][]Subscription` + mutex），进程重启即清空；浏览器
  下次启动会重新登记。不落库、不改 schema。
- **重复订阅幂等**：同一 endpoint 重复上报只更新、不重复存储。
- **失效订阅自动清理**：推送服务返回 404/410 时该订阅被删除；其它错误只记
  WARN，不影响主流程（推送永远不阻断消息投递）。
- **本批未接入每条消息**：Notify 目前只能被显式调用（测试入口 / 后续接线点）。
  后续批次会在 data 层 fanout 结果送达用户后触发 `a.push.Notify(uid, payload)`。

## 6. 相关文件

- 服务端实现：`server/internal/pushsvc/`（`pushsvc.go` 端点与订阅存储、
  `sender.go` VAPID + aes128gcm 加密、`subscribe_test.go` 单测）
- 接线：`server/cmd/chatd/assemble.go`（`newAssembly` 构造、`buildHTTP` 挂载
  `/api/push/`）
- 客户端契约：[client-contract.md §5](client-contract.md)（订阅上报与失败语义）
