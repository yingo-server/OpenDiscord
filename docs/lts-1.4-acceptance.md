# 1.4 验收与转生产锚点（代理用 file_read 完整读，按 §X 执行）

## 全局铁律
1. 禁止 git commit / push / tag；禁止 go build / go test / npm build / npm test / go vet；禁止装依赖；不改 go.mod/go.sum/package.json。
2. 允许：gofmt -l、阅读、rg、写代码/测试文件。临时脚本只写 /tmp。
3. 汇报 ≤8 行：逐文件一行 + 未完成清单 + 是否按时。60 分钟预算。

## §A CORE-3 补齐测试（单文件域 server/cmd/chatd/*_test.go）
读 docs/universal-pull-spec.md §3 兼容矩阵、server/cmd/chatd/v3deliver_test.go、server/cmd/chatd/adaptive.go、server/cmd/chatd/v3attach.go。
1. 控制器热生效单测：adaptive_control_enabled false→true 后 effective[cid] 经 AdaptiveWrite 生效；关断后 AdaptiveClear 回退静态 50/2000。
2. 灰度切换不双投递 e2e 断言：同 uid 一条消息在 flag-on 时只经一条路径（mailbox 或 legacy 之一）到达；flag-off 只走 legacy。用现有 startAssembly / sendAssemblyMsg。
3. 旧客户端隔离：universal_pull_enabled 开启时无 kind14 能力连接必须仍走 inline（不漏消息）。若现有代码无能力协商字段，则在测试里明确写出该前提并标「未实现·阻断生产启用」，**不要发明协议字段**。
4. 只碰测试文件，不改任何生产逻辑。

## §B 旧客户端能力协商（生产代码，文件域 server/internal/user/ + server/cmd/chatd/ + proto）
读 universal-pull-spec.md §3 兼容矩阵与 §5 回滚清单。
1. 客户端 HELLO/能力帧上报 `pull_capable`（bool）。服务端在连接注册表记该能力。
2. universal_pull_enabled=true 时：仅对 pull_capable 连接走水位线；无能力连接保持 inline 逐成员推（不漏消息）。
3. 不改 Envelope 现有字段号；能力上报复用已有 HELLO 载荷扩展或新增系统 kind（15-19 空号段择一），kinds.go / kinds.ts 同步登记。
4. S-7 回滚：flag-off 时能力协商零行为影响。
5. 测试：能力表读写、无能力连接 flag-on 仍 inline、flag-off 逐字节。
6. 禁止改 fanout 现有投递逻辑以外的东西。

## §C 验收编排（文档 + workflow，不改业务代码）
读 docs/lts-1.4-plan.md §6 发布门槛、docs/universal-pull-spec.md §4、.github/workflows/stress.yml、docs/lts-1.4-plan.md §9 验收记录表。
1. 写 scripts/stress/acceptance.sh（POSIX sh，bash -n 通过）：依次触发 S-1~S-7 七组 dispatch，收集结果 JSON，按门槛判定 PASS/FAIL，把结论写进 docs/lts-1.4-plan.md §9 表。
2. 门槛（逐条写进脚本常量，不改数字）：S-1 500人≥95%/p95≤2s、S-2 50人≥90%、S-3 512×10不劣于 v1.2.1 94.5%、S-4 慢消费者其余≥99%/数据帧丢失=0、S-5 重启风暴≤800QPS无5xx、S-6 自适应抖动≤2×、S-7 flag 回滚逐字节。
3. 不实际跑压测、不打 tag、不改 ci.yml。
4. 在 docs/lts-1.4-plan.md §9 表尾加「转生产判定」一行：全部 PASS 才允许把 4 个 flag 默认值改 true，改 flag 属最后单独提交。

## §D 客户端能力上报（转生产阻断项，文件域 apps/client/src/net/）
1. 连接 open 后（WsClient 的 onOpen / requestFeatureFlags 同处）发 HELLO(1) 或 CLIENT_CAPS(18) 上报 `pull_capable:true`。
2. 用 kinds.ts 已登记的 18；payload 与服务端 router/capability 解析一致（protobuf Hello.pull_capable 或 JSON {"pull_capable":true}）。
3. mockBridge 同步（不发能力帧时视为旧客户端，保持现有 mock 行为）。
4. 单测：上线即上报、能力帧只发一次、mock 不上报。
5. 不改 go.mod/服务端/sw.js。

## §E 双规格压测编排（文件域 .github/workflows/stress.yml + scripts/stress/acceptance.sh + docs/lts-1.4-plan.md）
1. CI runner 是 4c16g；要按 **2核2g** 与 **4核16g** 两档各跑一遍 S-1~S-7。
2. 2核2g 档：用 cgroups/taskset/`GOMAXPROCS=2` + 内存上限约束压测进程（容器内可用 `ulimit -v` 或 `systemd-run --scope` 不可用时，用 `/sys/fs/cgroup` 写 memory.max/cpu.max，失败则明确标 BLOCKED 不假跑）；4核16g 档用默认 runner 资源。
3. workflow 新增 `hw_profile` 输入（2c2g / 4c16g），acceptance.sh 对每档依次触发 S-1~S-7，结果表按两档分列。
4. 文档 §9 表改成两档 × 七行，结论列留空。不改 ci.yml、不打 tag、不实跑。

## §F 测试默认 flag 依赖清理（文件域 server/**/*_test.go）
1. rg 找出所有断言「默认 flag 为 false/percent=0」的测试（grey_test.go 已由主会话改）。
2. 凡需要 off 语义的测试，改为显式 `NewFromMap(map[...]{off})` 或 `flags.Apply(off)` 构造，不再依赖 Defaults()。
3. 新增 TestProductionDefaults：断言 4 个 flag 默认 true / percent=100 / msg_domain_v2=true。
4. 只碰测试文件，不改生产逻辑、不构建测试。
