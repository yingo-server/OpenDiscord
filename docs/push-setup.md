# Web Push（系统推送）部署与验证手册

适用范围：v1.5.0-beta.3 —— 移动端系统推送的**服务端**（订阅登记 + VAPID 发送）。

> 本批交付边界：服务端**订阅端点可用**（`POST /api/push/subscribe`）、
> **Notify API 可调用**（`pushsvc.Service.Notify(uid, payload)`）、以及一个
> **测试触发入口**（`POST /api/push/test`）。**尚未**接入"每条消息送达即推送"
> 的投递路径（该接线将在后续批次落到 fanout 之后），因此现在手机仍不会
> 收到每条消息的实时系统通知，只在手动触发测试推送时收到。

---

## 1. 生成 VAPID 密钥对

VAPID 密钥是一对 P-256（ECDSA）密钥：公钥给浏览器（`applicationServerKey`），
私钥只留服务端。用 OpenSSL 生成（任选其一）：

```bash
# 方式 A：PEM（SEC1）私钥 + 原始公钥
openssl ecparam -genkey -name prime256v1 -noout -out vapid.pem
openssl ec -in vapid.pem -pubout -out vapid_pub.pem

# 方式 B：直接从 PEM 导出 base64url 公钥（前端注入用）
openssl ec -in vapid.pem -pubout -outform DER 2>/dev/null | tail -c 65 | base64 | tr '+/' '-_' | tr -d '=\n'
```

上面 `tail -c 65` 取的是未压缩点（`0x04 || X || Y`），
再用 base64url（去掉 padding）编码，得到前端需要的 `VITE_VAPID_PUBLIC_KEY`。

公钥也可直接用 Node 生成（与前端同一份密钥，避免两边不一致）：

```bash
node -e "const c=require('crypto');const {publicKey}=c.generateKeyPairSync('ec',{namedCurve:'prime256v1'});\
console.log(publicKey.export({type:'spki',format:'der'}).toString('base64').slice(-65)||'')" 2>/dev/null
```

推荐：用 `web-push` 的官方工具（最省心）：

```bash
npx web-push generate-vapid-keys
# → { publicKey: "...", privateKey: "..." }
```

## 2. 前端注入公钥

前端从构建期环境变量读取（`apps/client/src/net/push/index.ts`）：

```bash
# apps/client/.env.production
VITE_VAPID_PUBLIC_KEY=<第 1 步得到的公钥（base64url，无 padding）>
```

未注入时前端 fail-closed：不注册 ServiceWorker、不请求通知权限、不上报订阅
（只 `console.info`），不影响 WS 长连接与页内通知。

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

前置：浏览器已登录（拿到 `access_token` Cookie），且当前是 **production 构建**
（dev/mock 模式下前端不会注册 SW）。

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

前端对任何非 2xx 都 fail-closed：只记日志、不弹错、不重试风暴。

### 4.2 端到端（真机）

1. 生产构建前端并注入 `VITE_VAPID_PUBLIC_KEY`，部署 HTTPS（Push API 强制 HTTPS，
   ServiceWorker 也要求安全上下文）。
2. 手机浏览器打开站点并允许通知权限。
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
- 前端契约：`apps/client/src/net/push/index.ts`
