# OpenDiscord 管理员指南

面向自部署 OpenDiscord 的服务器管理员。

## 部署

### 要求

- Linux 服务器（2C2G 起），systemd
- 域名，TLS 由 Caddy 自动管理
- 外部 PostgreSQL（冷存储）

### 初始化

```bash
sudo ./install.sh                    # 部署二进制/单元/配置/密钥
sudoedit /etc/opendiscord/opendiscord.toml # 填写域名、冷库连接、短信供应商
sudo systemctl enable --now opendiscord
```

首次注册的账号自动成为管理员。**务必立即**：管理后台 → 设置 → 开启 TOTP 二步验证、配置 IP 白名单。

## 日常运维

```bash
opendiscordctl status          # systemd unit 状态 + SIGUSR1 dump 摘要 + 热库统计（消息数/最新时间/文件大小）
opendiscordctl backup          # 立即备份（热库 + 配置 + 密钥清单）
journalctl -u opendiscord -f   # 跟随日志；-g trace_id=xxx 排查单条消息
```

热库 SQLite 保留最近 90 天，每日 04:00 自动归档至冷库，备份每日 05:30 自动执行（策略见 `/etc/opendiscord/policy.json`，改完 `systemctl reload opendiscord` 生效）。

## 内容治理

- **封禁**：后台用户页或 `opendiscordctl ban <uid> --level 2 --days 7`。封禁立即断开其全部连接并撤销登录凭证
- **举报**：后台举报队列按时间倒序；普通举报 24h 内处理，涉未成年人/人身威胁 2h 内升级
- **紧急下架**：涉即时人身危险内容，值班管理员单人可即时下架并全端撤回，事后 24h 补双人复核
- **广播**：后台编写 → 预览触达人数 → 发送；支持定时

## 应急

| 场景 | 操作 |
|---|---|
| 注册被刷 | 后台开关：禁注册（游客通道同步关闭） |
| 数据事故 | 后台开关：全站只读（可收不可发），随后排查 |
| 安全事故 | `sudo systemctl stop opendiscord`，防火墙只留 SSH，通知用户 |
| 单用户盗号 | 立即封禁（撤销全部登录凭证），联系用户核实后解封 |

以上开关须 5 分钟内可执行；全部操作自动写入审计日志（防篡改哈希链，每日校验）。

## 合规要点

- 审计日志保留 5 年；登录日志 180 天；用户注销有 15 天冷静期
- 用户申请数据导出须在规定时间内响应（后台导出队列）
- 举报证据加密保全，不可被普通删除流程清除

## 升级与回滚

> ⚠️ 未实现：`opendiscordctl upgrade` / `upgrade --rollback` 尚不存在（CLI 无该子命令）。升级/回滚请手工执行：

```bash
# 升级：替换二进制 + 滚动重启（连接优雅排空，用户无感）
sudo install -m 0755 /opt/opendiscord/current/bin/opendiscordctl /usr/local/bin/opendiscordctl
sudo install -m 0755 /opt/opendiscord/current/bin/chatd        /usr/local/bin/chatd
sudo systemctl restart opendiscord

# 回滚：切回上一版本二进制后重启
sudo install -m 0755 /opt/opendiscord/previous/bin/chatd /usr/local/bin/chatd
sudo systemctl restart opendiscord
```

详细架构与开发信息见 [开发文档](README.md)。

## 供应商接入与 TURN 自检（v1.4）

### 供应商适配器（短信 / 邮件 / 验证码）

三个通道均为**通用 HTTP webhook**：POST JSON 到配置 URL，超时 3 秒，非 2xx 即失败。阿里云/腾讯云等云厂商 SDK 不内置——自建轻量 relay 持有云厂商密钥，OpenDiscord 只见 webhook。示例模板见 `deploy/providers.example.toml`（不含真实密钥）。

部署步骤：
1. 复制示例并填入 relay 地址：`sudo install -m 0640 deploy/providers.example.toml /etc/opendiscord/providers.toml`
2. 在 `/etc/opendiscord/opendiscord.toml` 加一行 `providers_path = "/etc/opendiscord/providers.toml"`，然后 `systemctl reload opendiscord`
3. 未配置该文件（或某通道 url 为空）时，该通道保持 **fail-closed**：短信/邮件验证码照常签发但不投递，图形验证码请求按 `CAPTCHA_REQUIRED` 拒绝——绝不 panic，也绝不静默放行

### TURN 连通性自检

通话沿用「自备 TURN」策略。管理后台新增一键自检（挂在现有诊断面，`GET /api/v1/diagnostics/turn`，需 `metrics.read` 权限）：

1. 在 chatd 配置加 `turn_url = "turn:turn.example.com:3478?transport=udp"`（或 `turns:`）
2. reload 后在管理后台诊断页点击「测试 TURN 连通性」；TCP 探测为一次成功连接，UDP 探测发送一个最小 STUN Binding Request，收到任意合法应答即视为存活
3. 整体预算 3 秒；结果（ok / transport / addr / detail / duration_ms）只在管理面显示，不暴露凭据、不影响用户面
4. 未配置 `turn_url` 时返回 `ok=false, detail="turn not configured"`，不会报错
