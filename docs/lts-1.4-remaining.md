# 1.4 剩余批次实施锚点（代理用 file_read 完整读，按 §X 执行）

## 全局铁律（违反即失败）
1. **禁止 git commit / git push / git tag**——由主会话统一提交一次。工作区改动保留即可。
2. **禁止 go build / go test / npm build / npm test / go vet**——本地一律不构建不测试，验证在全部推送后由 CI 统一跑。
3. 允许且仅允许：`gofmt -l`（静态格式查看）、阅读文件、`rg` 检索、写代码与测试文件。
4. **不装任何 SDK/依赖/包**。不改 go.mod / go.sum / package.json / package-lock.json（除非 §X 明确授权）。
5. 临时脚本一律写在工作区外（/tmp），或写完即删；**绝不在仓库根留 *.sh 辅助文件**。
6. 汇报 ≤8 行：逐文件一行 + 未完成清单 + 是否按时。不复述任务书、不贴代码。
7. 每批 60 分钟预算；若范围超了，停下报告剩余，不赶工。
8. 任务书短、要求全在本文，代理不得自行扩大文件域。

## §1 CORE-3（v3 接线 + 自适应控制器接线 + 双投递保护）
文件域：`server/cmd/chatd/`（v3wire.go / v3deliver.go / v3hooks.go / assemble.go / 新增文件）、`server/internal/user/app/server.go`（已有 ConnLifecycle 钩子）、`server/internal/data/pipe/`（SetAdaptiveStore 调用点，只读不改逻辑）。
要求：
1. 读 `docs/v3-bus-design.md §3/§5/§6`、`docs/universal-pull-spec.md §2.2`、`docs/v3-p0-checklist.md §5`，理解现有中间态。
2. 把 `AttachV3Deliver` 真正接进 `newAssembly`：连接 OnConnect → `Subscribe` + 启动 `deliverLoop`；OnDisconnect → `Unsubscribe` + cleanup。复用 server.go 已有 `AddConnLifecycle` / `ConnLifecycle`，不改接口语义。
3. **双投递保护（最高优先级）**：灰度 uid 开启 v3 投递时，必须同时关闭该 uid 的旧逐成员直投，否则同一帧收两遍。实现为按 uid 的单一路径判定（flag 快照 + InBucket），一处判定两处共用；`v3_bus_enabled=false` 时旧路径逐字节不变。
4. kind 15 MAILBOX_ACK 上行：在 user→data FrameSource 侧解析累积 cursor，转调 `Plane.OnAck`（经 hub.Ack → Store.MarkAcked）。不新增 kind、不改 Envelope。
5. CORE-1 自适应控制器接线：chatd 控制循环（或 jobber 周期）读 `bus/adaptive` 的 `Tick`，把 `effective[cid]` 写入 `pipe.SetAdaptiveStore`；`adaptive_control_enabled=false` 时不写、保持静态默认。
6. 背压指标：`Plane.Stats()` 的 PausedConns / Backpressured 经现有 dump（SIGUSR1）/ metrics 暴露，不新增 HTTP 端点。
7. 测试（新增文件）：T-SV-5 逐字节 flag-off 回归、灰度切换不双投递、kind15 上报推进 cursor、控制器热生效。全部用现有 startAssembly helper。
8. 不改 `deploy/`、不动 `apps/`、不改 go.mod。

## §2 FE-2（PWA + Web Push）
文件域：仅 `apps/client/`（`public/` 新增、`src/` 新增模块、`index.html` 一处挂载）。
要求：
1. 读 `docs/lts-1.4-plan.md §5` 第 6 行。
2. `public/manifest.webmanifest`（name/short_name/start_url/display=standalone/icons 占位/theme_color 对齐现有设计令牌）+ `public/sw.js`（install/activate/fetch：只缓存静态壳与已缓存媒体，**不缓存 /api/ 与 /ws**；离线回退 index.html）。
3. `src/net/push/`：VAPID 订阅封装（subscription → POST 到服务端接口缝 `/api/push/subscribe`，服务端未实现时前端 fail-closed 只记日志，不弹错）；push 事件 → notification + 点击 focus 会话。
4. `index.html` 挂 manifest + 首帧前注册 service worker（`production only`，dev/mock 模式不注册）；不破坏既有「不闪白」内联脚本。
5. i18n key 化全部新文案；`prefers-reduced-motion` 无关项。
6. 单测（纯逻辑）：SW 缓存策略判定、push 未配置时 fail-closed、dev 不注册。用 `node_modules/vitest` 既有写法，**不新增依赖**。
7. 不改任何服务端文件、不改 package.json。

## §3 OPS-2（供应商适配器 + TURN 自检）
文件域：`server/internal/authsvc/`（适配器与配置读取）、`server/internal/admin/`（TURN 自检 handler）、`deploy/`（示例配置模板，仅新增文件）、`docs/admin-guide.md`（一节说明）。
要求：
1. 读 `docs/lts-1.4-plan.md §5` 第 2/7 行。
2. authsvc：定义 `SMSSender` / `Mailer` / `CaptchaVerifier` 的**可插拔 HTTP webhook 实现**（通用：POST JSON 到配置 URL + 超时 3s + 非零即失败）；配置缺失时保持现有 fail-closed（不发送、不抛 panic）。新增 `deploy/providers.example.toml`（阿里云/腾讯云/通用 webhook 三段示例，**不含任何真实密钥**）。
3. admin：TURN 连通性自检（读配置里的 turn URL，做一次 UDP/TCP 可达性探测，超时 3s，结果只在管理面显示；不引入新依赖，用标准库 net）。挂到现有健康检查/诊断路由，不新增端点前缀。
4. 全部配置读取经现有 config 机制，代码内零硬编码供应商常量。
5. 单测：webhook 成功/超时/非零、未配置 fail-closed、TURN 探测超时。
6. 不改 go.mod、不动 apps/、不改 CI。

## §4 DOC（文档收敛）
文件域：`docs/known-issues.md`、`CHANGELOG.md`、`README.md`、`docs/README.md`、`docs/roadmap.md`。
要求：
1. 读 `docs/lts-1.4-plan.md §5`（12 条决策）与 §6（发布门槛）。
2. known-issues 逐条更新为「已修 / 保持（写明理由）/ 延后 v1.5」三态，**不许把未验证的代码标成已修**：凡 flag 默认关闭、未压测、未接线的，一律标「代码在位·待验证」。
3. CHANGELOG 新增 `[1.4.0]` 段：按 Added/Fixed/Experimental 三类，Experimental 段明确列出全部 flag-off 与未接线项。
4. README 容量表标注「1.4 数据待 S-1~S-7 压测刷新」，保留 v1.2.1 实测数字不删。
5. docs/README.md 索引补 `lts-1.4-plan.md` / `universal-pull-spec.md` / `v3-bus-design.md`。
6. 明确不做清单（朋友圈/支付/公众号/自建 SFU/split/E2EE）单列一节。
7. 只写文档，不碰代码、不改版本号到已发布字样。

## §5 REL（压测场景与发布准备，不打 tag）
文件域：`scripts/stress/`、`.github/workflows/stress.yml`、`docs/lts-1.4-plan.md`（仅追加「验收记录」空表）。
要求：
1. 读 `docs/universal-pull-spec.md §4`（S-6/S-7）与 `docs/lts-1.4-plan.md §6`（S-1~S-7 门槛）。
2. scripts/stress 增加 `--universal-pull` 模式：所有会话按 kind 14 水位线驱动拉取（复用现有 pullTracker）、统计水位线/秒、SYNC QPS、收敛延迟、下行帧三分；增加 S-4 慢消费者注入（按比例限速连接）与 S-5 重启风暴（已有 storm.go 复用）。
3. 自适应断言：同场景跑 static vs adaptive 两组，输出 batch/interval 生效样本与速率限制命中数，供 S-6 判定。
4. workflow 增加对应 dispatch 输入（沿用现有输入风格，YAML 合法），**不改 ci.yml**。
5. 不实际打 tag、不改 release job、不跑压测。
6. 在 lts-1.4-plan.md 末尾追加空「验收记录」表（S-1~S-7 七行，结论列留空待 CI 填）。
