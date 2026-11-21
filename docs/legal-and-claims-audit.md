# UFOT v2.0.0 版权与声明合规审计（legal-and-claims-audit）

> **状态更新（2026-11-22）**：项目已由 OpenDiscord 更名为 **UFOT（Unfettered Freedom Of Thought）**。
> 下文第 1 节「商标风险」针对的是**更名前**的 OpenDiscord 名称；更名后项目名不再包含
> "Discord" 字样，该商标近似风险已消除，README 中的旧免责声明已相应移除。
> 第 2 节（第三方许可证）与第 3 节（声明可证实性）结论仍有效。

- 审计角色：发布前 copyright/claims accuracy/legal 层（sota-copywriting rules/04 重点、rules/01 声明段）
- 审计对象：README.md、docs/release-notes-v1.4.0/v1.5.0/v2.0.0.md、LICENSE、deploy/release.sh、go.mod、apps/client 与 apps/admin 的 package.json
- 方法：只读审计；证据来自本仓库文件、`docs/lts-1.4-plan.md §9`、`docs/v2.0-roadmap.md`、`.github/workflows/`。未跑 build/test，未 commit/push/tag。
- 说明：本报告为工程合规自查，非法律意见；标注"需核实"的结论须由法务/许可证原文确认。

## 1. 商标风险：项目名 "UFOT" 与 Discord

**事实陈述（可证实部分）**
- 仓库中不存在任何 "Discord" 商标授权、关联声明或共同品牌文件（`rg -i discord` 除项目名外无命中；无 logo/品牌素材、无第三方署名文件）。
- LICENSE 第 6 条明确：本许可证不授予使用 Licensor 的商号/商标/产品名称的权利；Apache-2.0 本身不解决与第三方（Discord）的商标冲突。
- README 通篇未声明 "UFOT 与 Discord 无关"、"非官方/未授权"，存在被误认为 Discord 关联/开源版本的混淆风险（商标淡化/搭便车风险，**需核实**法务意见）。

**结论（一句话）**：`UFOT` 与 `Discord` 高度近似且无授权证据，发布前应在 README 显要位置加入"非 Discord 关联、未获商标授权"的免责声明，由法务确认是否需更名（本次审计不改任何原文）。

**建议加入 README 的免责声明文本（仅建议，不改文件）**：
> 商标声明：UFOT 是一个独立开发的自托管即时通讯项目，与 Discord Inc. 及 Discord 品牌**没有任何关联、背书或授权关系**。"Discord" 及 Discord 标识是其各自权利人的商标；本项目名称中的 "Discord" 字样仅用于描述性目的（指代类似的使用场景），不主张任何商标权利。如权利人提出异议，我们将配合调整项目名称。

## 2. 第三方许可证清单（直接依赖，Apache-2.0 兼容性）

依据：go.mod（v1.22，5 个直接 Go 依赖）+ 两应用 package.json（client 4 个、admin 2 个运行时依赖，去重后前端 4 个）。合计 **9 个直接依赖**。以下许可证类型基于版本/常识判断，标注"需核实"的以许可证原文为准。

| 依赖 | 版本 | 许可证（常识） | 与 Apache-2.0 兼容 | 备注 |
|---|---|---|---|---|
| github.com/golang-jwt/jwt/v5 | v5.2.1 | MIT | 兼容 | 宽松许可，保留版权声明即可 |
| github.com/gorilla/websocket | v1.5.3 | BSD-2-Clause | 兼容 | 需保留版权/免责声明 |
| github.com/jackc/pgx/v5 | v5.6.0 | MIT | 兼容 | 保留版权声明 |
| github.com/mattn/go-sqlite3 | v1.14.22 | MIT（isclimited 双许可需核实） | 兼容（需核实） | 以仓库 LICENSE 原文为准 |
| golang.org/x/crypto | v0.24.0 | BSD-3-Clause | 兼容 | 保留版权声明 |
| react（client+admin） | 18.3.1 | MIT | 兼容 | 含 react-dom |
| react-dom（client+admin） | 18.3.1 | MIT | 兼容 | — |
| zustand（client） | 4.5.2 | MIT | 兼容 | — |
| @tanstack/react-virtual（client） | 3.5.0 | MIT | 兼容 | — |

**摘要**：9 个直接依赖，全部为 MIT/BSD 类宽松许可证，与 Apache-2.0 **兼容**（其中 go-sqlite3 许可证细节标注需核实）；但兼容性不等于可忽略署名——Apache-2.0 §4(c)/(d) 及 MIT/BSD 条款要求在再分发中保留各依赖的版权与许可文本。当前仓库**无项目级 NOTICE 文件、无 THIRD-PARTY-NOTICES、README/LICDENSE 未列出这些署名**（仅在 node_modules 构建产物中存在 typescript 自带的第三方声明，不构成合规署名）。

## 3. 声明可证实性审计（accuracy gate）

逐条核对客观声明与 CI 证据链；证据引用格式：`lts-1.4-plan.md §9` 与 `v2.0-roadmap.md` 的 run id。

**3.1 有 CI 证据支持的客观声明（可证实）**
- README/发布说明中的容量数字（512×10 98.1%/91.7%、1000×50 32.3%/23.7%、约 3500 msg/s、40k 帧/秒、1000/1000 握手）——对应 v1.1.1 基线测试，方法在 `docs/perf-spec-v1.2.md`、`scripts/stress/main.go`；数据由 CI 压测产出，属**有证据**（历史结果 JSON 部分已清理，引用 lts-1.4-plan §9 记录）。
- v1.4 知情发布的 S-1 0.93%、S-2 16.38%、S-3 93.86%、PASS=0/FAIL=6/BLOCKED=8——证据：`lts-1.4-plan.md §9`，run_id `44195161073`（S-1/S-2/S-3 4c16g）、`44193374341,44193623078`（S-2 2c2g）等，证据目录 `/tmp/acceptance-20261117T224653Z`。**有证据**。
- v2.0 的 ACK 1197/s、可用性 17.54%、p95 471ms、确认 35909 条、"+107% 翻倍"——证据：`v2.0-roadmap.md` run `44879921690`（第二次）及第一次 `44878897175`；v1.5 基线 579/s 为记录值（原证据目录已清理）——**有证据，但基线需核实/标注为记录值**。
- CI 全绿（-race + 前端构建）——`.github/workflows/ci.yml`、`stress.yml` 存在，README/release notes 与仓库配置一致；具体某次 run 链接未在文案给出（属弱证据，建议补链接）。

**3.2 无充分证据或证据不足的声明（需在发布材料补证）**
- README 第 3 行 "数据在你自己的服务器上，**不经过任何第三方**"：与代码冲突 —— `apps/client/src/net/call/peer.ts:34` 默认 ICE 服务器为 Google STUN（`stun:stun.l.google.com:19302`），系统推送经浏览器厂商 push 服务，`deploy/providers.example.toml` 通过 webhook 接第三方短信/邮件 relay（可指向 Cloudflare Worker 等）。这些场景下元数据或消息通知会经过第三方。**该绝对化声明无法证实，属夸大/误导风险**。建议改为："聊天内容默认存储于你自托管的服务器；语音视频通话默认使用公共 STUN 服务发现地址（可配置为自建 TURN），系统推送经浏览器厂商推送网络，验证码可通过自建 relay 发送。"
- README 第 5 行 "**全部装进一个二进制**"：部署产物含独立前端 dist（web/client、web/admin），非单一二进制文件；应改为"服务端与前后端由一套发布包提供"。**证据不足**。
- README 第 5 行 "**五分钟装好**"：无计时基准；属 friction reducer，需实测或改为"快速开始"。**无证据**。
- README 第 21 行 "审计日志哈希链 + append-only，**不可篡改**"：绝对化措辞；若有权管理员可改写存储，则该词不实。建议改为"篡改可被检测"。**超节表述**。
- 用户指南（非审计目标文件，供交叉核对）列出"微信/Apple/运营商一键登录"，但代码 `AuthProviderId` 仅 `phone/email/password/guest`，属未实现功能的前置文档；README 未列此项，无需改 README，但提示事实一致性风险。**需核实**。

**3.3 夸大/超节清单（原文位置 → 建议改写，均不改原文）**
| 位置 | 原文 | 问题 | 建议改写 |
|---|---|---|---|
| README.md:3 | 不经过任何第三方 | 与 STUN/push/relay 冲突 | 见 3.2 建议文本 |
| README.md:5 | 全部装进一个二进制 / 五分钟装好 / 不需要容器 | 发布包含独立 web 资产；无计时依据 | "一套发布包，含前端资产"；"快速开始"；不提容器 |
| README.md:21 | 不可篡改 | 绝对化 | "篡改可检测" |
| README.md:11,215 + rn-v2.0:23,44 | 写吞吐翻倍 / +107% | 有 run 44879921690 支撑，但基线为记录值（证据已清理） | 保留，但补 run 链接并将基线标注"历史记录值，证据目录已清理" |
| release notes v1.4/v1.5/v2.0 | "知情发布" "本版不宣称容量达标" | 措辞诚实，无需改写 | 保留 |

**审计计数**：有证据 6 类（容量基线 5 组、v1.4 S-1~S-3、S-2 极限值、v2.0 三组数据、CI 配置存在性、数据结构事实），无充分证据 4 处（不经过任何第三方、全部装进一个二进制、五分钟装好、用户指南第三方登录），夸大/超节 3 处（不经过任何第三方、全部装进一个二进制、不可篡改）。

## 4. 发布前 Legal Checklist

- [x] LICENSE 完整性：根目录 `LICENSE` 为 Apache-2.0 全文 202 行，含附录样板；§6 商标条款存在。完整。
- [x] NOTICE 是否需要：Apache-2.0 §4(d) 仅在 Work 自带 NOTICE 时才强制下游附带；本项目无自有 NOTICE。法律上可无，但**强烈建议新建 NOTICE**（建议做）。
- [!] 第三方署名：9 个 MIT/BSD 依赖的版权/许可文本未随发布包/仓库展示（Apache-2.0 §4(c)/(d) + 各依赖许可证要求）；建议新建 THIRD-PARTY-NOTICES.txt 或把清单写入 NOTICE（**发版前阻断**：需至少在源码分发中保留依赖许可证文本）。
- [!] 商标免责声明：README 缺"非 Discord 关联/未授权"声明（**发版前阻断**）。
- [~] 性能声明证据链：性能数字有 run id，但正文无直接链接、v1.5 基线为记录值（证据已清理）（建议做：补 CI 链接/标注记录值）。
- [x] 用户数据/PII 声明：本项目不向项目方自建后端采集 PII（代码无 telemetry/analytics SDK，`docs/security-spec.md` 多处 PII 红线）；但"数据不经过任何第三方"因 STUN/推送/relay 不成立（见 3.2）。README 说"数据在你自己的服务器"就托管模型而言成立，不要扩写为"不经任何第三方"。
- [x] 暗黑模式：审计目标文件无 confirmshaming/假紧迫/预选同意等模式（rules/04 §4 not applicable，理由：无相关界面/营销文案受审）。

## 5. 结论（发版前处置）

**必须做（发版前阻断）**
1. README 加入 Discord 商标免责声明（措辞见 §1）——混淆风险。
2. 新建 NOTICE / THIRD-PARTY-NOTICES，列出 9 个直接依赖及其许可证文本或获取方式——Apache-2.0 §4(c)/(d) 与 MIT/BSD 署名义务。

**建议做（非阻断）**
3. README "不经过任何第三方" "全部装进一个二进制" "五分钟装好" "不可篡改" 按 §3.3 改写，避免绝对化误导。
4. 性能数字补 CI run 链接（lts-1.4-plan §9 / v2.0-roadmap 的 run id），将 v1.5 基线 579/s 标注为"历史记录值，证据已清理"。
5. 法务确认 `UFOT` 名称是否需规避 Discord 商标（可能涉及更名）。
6. 核实 go-sqlite3 许可证原文（MIT 与 ISC 双许可）。

**无需做**
7. 修改 LICENSE 正文（Apache-2.0 全文完整）。
8. 改动 CI 压测数据、release notes 实测数字或任何代码/配置（现有数据诚实，问题在文案与署名包装）。
9. 为项目方自建 PII 采集添加隐私政策（项目不采集；实例级隐私政策由部署方提供，见 docs/user-guide.md）。

**修改文件清单**：本次仅新建 `docs/legal-and-claims-audit.md`；未改动 README/release notes/LICENSE/任何代码；未执行 build/test；未 commit/push/tag。改动留在工作区。
