# 部署与验收方案（开源通用形态）

## 原则

1. **零硬编码域名**：前端源码只有 `.env.example` 模板；`.env.production` 由部署者按实际域名填写，且**不入库**（加进 .gitignore）。`deploy/` 下提供 `.env.production.example` 作为文档示例（example.com 占位）。
2. **构建只允许 CI**：给 CI 加一个 frontend 构建任务，产物挂到 Release（同后端产物一起），服务器只下载。
3. **分用户**：前端静态文件归 `ufot:ufot`，放 `/var/lib/ufot/frontend/`（与后端数据同属主，权限 755 只读服务）。
4. **不影响其他服务**：Caddyfile 只**新增**一个 UFOT 站点块，原文件其他部分不动；先 `caddy validate` 再 reload。

## 服务器侧改动清单（先给你过目，批准后执行）

### 1. `/etc/ufot/ufot.toml` 一行
```toml
public_origin = "https://server.344977.xyz"
```
（ reload 后自动 `SecureCookies=true`，这是代码既有逻辑 assemble.go:346）

### 2. `/etc/caddy/Caddyfile` 替换 UFOT 站点块
```caddy
server.344977.xyz {
	# 前端静态（同域托管 → Cookie/WS 同站，SameSite=Lax 即可）
	handle {
		root * /var/lib/ufot/frontend
		try_files {path} /index.html
		file_server
	}

	# 用户面 API + WS
	handle /api/* {
		reverse_proxy 127.0.0.1:3004
	}
	handle /ws {
		reverse_proxy 127.0.0.1:3004
	}
	handle /healthz {
		reverse_proxy 127.0.0.1:3004
	}

	# 管理面 REST（回环 7781）
	handle /api/v1/* {
		reverse_proxy 127.0.0.1:7781
	}

	header {
		X-Real-IP {remote_host}
		X-Forwarded-For {remote_host}
		X-Forwarded-Proto {scheme}
		Strict-Transport-Security "max-age=15552000"
	}

	log {
		output file /var/log/caddy/ufot-access.log {
			roll_size 20mb
			roll_keep 5
		}
	}
}
```
HTTPS 由 Caddy 自动签发（需要 80/443 端口可从公网到达——待确认防火墙/安全组）。

### 3. 前端产物分发（CI）

`.github/workflows/ci.yml` 增加 frontend job（tag 触发时随 Release 发布 `ufot-web-dist.tar.gz`）。服务器侧用现有 releases 目录模式：
```
/var/lib/ufot/frontend/   ← 解压 dist
```

## 代码侧改动（frontend/ 内，全部通用化）

- `.env.production` → **删除**（不入库），加进 `.gitignore`
- `deploy/frontend.env.example`（新）→ example.com 占位
- `README.md` 部署章节补同域/跨域两种形态说明

## 批准后执行顺序

1. CI 改动 commit（frontend job）→ 你审查后推 tag → 下载产物
2. 服务器：写 Caddyfile → validate → reload → 传产物 → 改 ufot.toml → reload ufot
3. 浏览器实测验收 2/3/4 → NOTES.md
