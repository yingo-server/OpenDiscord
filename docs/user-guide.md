# UFOT 服务使用者指南

面向**服务使用者**（部署了 UFOT、要用 API/WS 接入客户端的人）。本文不再描述某个具体
客户端界面的按钮流程；客户端交互契约见 [client-contract.md](client-contract.md)，
HTTP 字段级细节见 [api-reference.md](api-reference.md)，WS 帧见 [ws-protocol.md](ws-protocol.md)。

## 1. 部署服务

1. 到 [Releases](https://github.com/yingo-server/UFOT/releases) 下载产物并校验 SHA256。
2. `sudo ./deploy/install-interactive` 交互式安装（无 TTY 自动降级为参数模式）。
3. `sudo systemctl enable --now ufot` 启动；`curl -s https://你的域名/healthz` 应返回
   `{"ok":true,...}`。
4. 服务器只提供 API/WS，不托管 UI。详细步骤（Caddy、备份、升级）见 [deploy.md](deploy.md)。

## 2. 验证服务可用

```bash
curl -s https://你的域名/            # 服务描述符（surface 发现）
curl -s https://你的域名/healthz     # 健康检查
curl -s https://你的域名/api/auth/methods   # 当前开放的登录方式
```

## 3. 接入一个客户端

客户端是独立程序（官方 Web 前端、自研桌面/移动 App 均可）。接入只需三步：

1. **配置两个地址**：API base（如 `https://chat.example.com`）与 WS url
   （如 `wss://chat.example.com/ws`）；推荐作为客户端 env/配置项暴露。
2. **登录**：先 `GET /api/auth/methods` 探测开放的登录方式（四种：手机验证码 / 邮箱
   魔法链接 / 账号密码 / 游客；由服务端 `policy.json` 的 `auth.methods` 控制，默认全部
   关闭——部署后至少要打开一种）。登录成功得到会话对象与 `Set-Cookie: access_token`。
3. **建连收发**：带 Cookie 握手 `wss://…/ws`，按 ws-protocol.md 收发帧；
   媒体走 `POST /api/media/upload`；推送订阅走 `POST /api/push/subscribe`。

完整流程、错误码与限流语义见 [client-contract.md](client-contract.md)。

## 4. 账号与登录方式（服务端行为）

- 四种登录方式由 `policy.json` 热更开关控制，被关闭的方式调用即
  `403 METHOD_DISABLED`。
- 游客模式无需注册即可获得会话，之后可通过 `POST /api/auth/password/set`（≥10 字符）升级为正式账号。
- 多设备：默认单用户 3 台并发设备；`POST /api/auth/refresh` 轮换令牌；
  `POST /api/auth/logout` 登出当前设备。
- 验证码有频率限制（60s 间隔、单号 5/h + 10/day、单 IP 5/h）；密码登录连续失败 5 次锁定。

## 5. 常见问题

**收不到验证码？** 触发了发送限流（见上）；管理员可查 `identity_login_history` 与服务日志。

**消息发不出去？** 看 `MSG_ACK` 的 status（非 0 = 失败，原因见帧内信息）或 `13 RATE_LIMITED`
的 `retry_after_ms`；按退避重试，不要固定间隔轮询。

**跨域客户端连不上？** 检查服务端 `cors_mode`（默认 `allow_any`）与 WS 的
`allow_any_origin`；见 deploy.md §CORS。

**推送不工作？** 需服务端配置 VAPID（见 [push-setup.md](push-setup.md)）；未配置时订阅
照常登记但不投递。

---

*数据由部署 UFOT 的服务器管理员掌控。隐私政策以你所用实例的公告为准。*
