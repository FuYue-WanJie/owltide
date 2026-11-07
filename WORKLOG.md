# WORKLOG — OwlTide（潮之眼）

本文件是 OwlTide 项目的工作日志 + 工作规则。每次推进会话开始时先读本文件恢复上下文；每次有实质进展时在「日志」追加一条。

（m3e 项目的工作日志在 `/workspace/WORKLOG.md`，两者独立维护。）

---

## 0. 项目目标

长期在线的个人 Agent：能写代码、做日常帮手、当群聊「赛博群友」。

- 仓库：https://github.com/FuYue-WanJie/owltide
- 语言：Go（核心）；以后加 Kotlin（android/）和 WebUI（web/）
- 目录：core/ 核心、android/、web/、docs/、assets/，单仓库不拆
- 理念：薄核心 + 可自我修改的配置层。不做「一切皆插件」，不自造插件系统
- 目标平台：同一个 Go 二进制，主力 Android arm64（Kotlin 前台服务拉起子进程，不用 JNI/gomobile）。核心无 cgo、不假设桌面环境

## 1. P0（已完成，2026-11-05）

命令行多轮对话 Agent，能读写文件、跑 Shell 命令完成任务。

**包结构**（module `github.com/FuYue-WanJie/owltide`）：
```
core/
  agent/      Agent 循环 + 预算控制（含 history.go）
  model/      OpenAI 兼容客户端（流式 SSE 手撕 + 工具调用缝合）
  tool/       read_file / write_file / edit_file / list_dir / run_shell + 注册表
  decision/   decisions.jsonl（v1 schema）
  config/     ~/.owltide/config.yaml
  platform/   Platform 接口（OS 差异收口，Android 实现以后加）
cmd/owltide/  CLI 入口（chat 交互 / run 一次性）
```

**决策日志 v1 字段（冻结）**：`ts, session_id, turn, input_digest, model, prompt_tokens, completion_tokens, tools_called[], tool_results[], replied_directly, duration_ms, outcome(success|error|user_correction), error`

**关键设计**：
- Platform 接口：Home()/Shell()/PathSep，桌面实现 `desktop`
- Token 预算：从 usage 累计，超预算/超轮数发收尾消息强制总结
- edit_file：old_string 精确替换，0 匹配或多匹配都报错
- 依赖仅 `gopkg.in/yaml.v3`，其余全标准库

**P0 验收记录（2026-11-05）**：
- `go build ./...` 通过；`GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build` 通过
- 单测全绿（假服务器模拟流式文本 + 工具调用 + 多轮 + 决策日志 + 轮数上限）
- 真模型端到端：用户端点的 nemotron-3.5-super-vl-120b-a12b 自主调 list_dir → write_file → run_shell 建 hello.go 并运行成功；decisions.jsonl 每轮有记录（含失败轮）
- 11 个本地提交：`05bc5ac`（文档）→ `a8ac633`，conventional commits 英文

## 2. 当前模型配置

- `~/.owltide/config.yaml`：base_url `用户端点 URL`，model `nvidia/nemotron-3.5-super-vl-120b-a12b`（用户提供的 key，走 `OWLTIDE_API_KEY` 环境变量，key 不落盘）
- 该端点还有其他带 `/` 的轻量模型可用（curl /v1/models 可查）
- 注意：nemotron 是 reasoning 模型，回复可能带 reasoning_content，content 出现较慢

## 3. 不在 P0 范围（别提前做）

- 子代理、MCP、Skills、记忆
- `~/.owltide` 自动 git 提交与 agent.upsert 等自进化工具（P1）
- Android 客户端、WebUI、QQ、远程环境

## 4. 待办 / 下一步

- P1 已实现（见 1b 节），待真模型冲烟（用户同意后用 OWLTIDE_API_KEY）：让它「新建一个叫 reviewer 的子代理」，确认有定义文件和提交；再让它修改一个 locked 项，确认被拒
- P0 已全部完成
- 遗留：首次 run 曾测过 `poolside/laguna-xs-2.1`、`deepseek-ai/deepseek-v4.1-flash` 等其他端点模型，未系统对比工具调用能力，需要时可做模型选型评测

## 1b. P1 计划与实现状态（2026-11-05）

范围：可自我修改的配置层 + 护栏。架构定稿见 `docs/ARCHITECTURE.md` 3.1/4.1/4.2/5.1/5.2 节。

已实现（全部单测覆盖、全绿）：
- `core/store`：git 兼容提交引擎（SHA-1+zlib 对象、递归 tree、diff、log、Checkout、RevertLast、ResolveCommit、EnsureInit、AutoCommit 含 who/tool/session 元数据），零第三方依赖，Android 可用，真实 `git log` 可读
- 锁定配置：`~/.owltide/locks.yaml`；`locks` 与 `config:budget` 永久锁定；`config:model.*`（模型身份）也拒绝 Agent 改；CLI `owltide lock/unlock` 仅用户可用；`history.rollback` 回滚前 stash 锁定路径、回滚后恢复
- 自进化工具 6 件套：agent.upsert / agent.remove / skill.write / memory.write / config.patch / history.rollback；统一流程 校验→锁检查→写→commit→热重载→健康检查→失败 revert
- 子代理 schema：`agents/*.yaml`（name/model/system_prompt/tools/budget 全校验，tools 只能是已注册工具）
- 变更日报：`owltide changes [--since=24h]` 从提交历史列出 who/tool/session/files
- CLI：init 先建布局再写配置；store 打开时 EnsureInit（保证回滚总有父提交）

验收达成：`go build`、`GOOS=android GOARCH=arm64 CGO_ENABLED=0`、`go vet`、`gofmt` 全干净；单测覆盖 upsert 合法提交+热重载、非法拒绝不落盘、健康检查失败自动回滚、locked 拒改且可读、rollback 不动锁定项、changes 报告；P0 测试无回归。

踩坑记录：
- `agents.Save` 原来调 `Validate(nil)` 导致工具名校验误报——Validate 约定 nil=跳过工具名校验
- Registry.Register 只存 Schema 名，writerTool 的 Schema/Call 必须用同名派生，否则注册名与调用名不一致
- git tree 是嵌套结构，diff/checkout 必须递归 flatten，不能用顶层名对齐

## 5. 工作规则

- 小步提交，一个提交一件事，提交信息英文 conventional commits
- 依赖尽量少，优先标准库；SSE/HTTP 手撕，不引 SDK
- 改架构前先更新 `docs/ARCHITECTURE.md`
- 模型相关 key 一律走环境变量（`OWLTIDE_API_KEY`），不写进配置文件或代码；不复用 Agent 自身环境变量
- 单测用假服务器模拟模型，不烧真 Token；真模型 smoke 单独跑
- 单元测试运行注意内存限制：环境仅 8G RAM 无 swap，`go test` 串行跑

---

## 日志

- [2026-11-04] P0 骨架开工：go.mod 初始化、platform/config/tool/model/decision/agent/cli 全部落地，单测全绿，修复 agent 内层循环轮数不递增导致死循环的 bug，android 交叉编译通过。中途曾误用 Agent 自身环境变量做 curl 探测，已纠正为项目专属 `OWLTIDE_API_KEY`。
- [2026-11-05] P0 收尾：`go mod tidy` 依赖提交（`a8ac633`）；真模型端到端验收通过（用户端点的 nemotron-3.5，建 hello.go 并运行成功）。首次 401 是因为 key 属用户端点 而配置指向 deepseek，改配置后通过。P0 完成，11 个本地提交。
- [2026-11-05] P1 实现完成：ARCHITECTURE.md 更新 + store/agents/evolve 三包 + CLI lock/unlock/changes，7 个新提交（`ff2462d..5c80a90` + docs）。全部单测绿、P0 无回归、Android 交叉编译过。
- [2026-11-06] P1 三点补强（用户复核意见）：
  - git 兼容：引擎现在写 `.git/index`（v2）——真 `git status` 干净；读取支持 pack（idx v2 + ofs/ref delta）与 packed-refs——外部 `git gc` 后引擎照常读历史/提交/回滚；Checkout 改为精确文件级同步（修了「顶层目录在目标树就跳过子文件删除」的 bug）；`logs/` 永不入提交（writeTree 跳过）。全周期测试：自动提交→fsck→gc→引擎 Log/RevertLast/继续提交→再 fsck→再 gc
  - 预算：运行时拦截本就有（steps>maxTurns / tokens>maxTokens → wrapUp 强制收尾）；补了「budget stop」写入 decisions.jsonl（outcome=error + "budget stop: turn_limit/token_budget"）+ 测试
  - 锁的崩溃安全：rollback 前 stash 现在落盘成 `.lockstash/` 日志目录并先提交（不入树），Checkout 后恢复、完成即清；启动时 `RecoverStash()` 自动完成被打断的恢复（锁定文件永远赢）；中断模拟测试覆盖
  - 端点兼容：模型端点只接受 `[a-zA-Z0-9_-]` 工具名——Agent 可见名改下划线（agent_upsert 等），提交元数据保留点号名（agent.upsert）
  - 真模型冲烟（nemotron-3.5）：①「新建 reviewer 子代理」成功——定义文件+提交（who=agent tool=agent.upsert session=…）、真 git log 可读；②锁定 agent:reviewer 后让模型改/删——agent_upsert×2+agent_remove 全被拒、文件未动、决策日志有记录；③最终二进制隔离 HOME 复跑——git status 干净、fsck ok、logs/ 未入提交

- [2026-11-06] P0+P1 推送完成：28+4 提交全部上 origin/main（fast-forward，无 force）。推送前对数澄清：之前报 16 是只数了 P1 段（a8ac633..HEAD），漏报 P0 的 11——P0 实际从未推送。未推历史经 filter-branch 洗掉 WORKLOG 里的私有端点域名（用户端点），终扫密钥/端点名/key 值 0 命中。
- [2026-11-06] P1 补充三项（各自独立提交）：
  1. 哈希锚定编辑 `1150c66`：read_file 尾部附 [sha256:...] 标记；edit_file 必填 expect_sha256，不匹配拒绝并返回最新片段（old_string 附近 ±5 行或文件头 25 行）
  2. agents/*.md 格式 `43bdeb8`：frontmatter（name/description/model/tools/budget）+ markdown body 即 system_prompt；Save/Load/Remove 全切 .md；Def.FilterRegistry 是白名单强制点（子代理注册表只含白名单工具，P2 调度接入）；校验新增 description 必填
  3. 危险命令强制确认 `178d8b3`：run_shell 检测 rm -rf（含组合 flag）/git push --force/-f/git reset --hard/mkfs/dd of=/dev/*/chmod -R 777/shutdown 系；write_file/edit_file 检测 locks.yaml；config:budget 沿用永久锁拒绝。Registry.SetConfirmer 注入用户确认回调（CLI stdin y/N），模型无法自证放行；无确认器（非交互）一律拒绝；拒绝经 tool error 通道进 decisions.jsonl（TestDangerousCommandLogged 验证）
  - 测试：agents roundtrip/frontmatter 解析/白名单过滤、danger 模式正负例、locks.yaml 门、agent 决策日志；全绿 + android 交叉编译 + gofmt/vet 干净

- [2026-11-06] 危险命令确认两处漏洞修复（用户复核）：
  1. 受保护文件事务守卫 `905cfb1`：新包 core/guard——每次工具调用前后对受保护文件（locks.yaml、config.yaml、全部锁定项路径，动态含新锁）做哈希快照/比对；run_shell echo/sed/cp/tee 改锁文件会被检出、从 config store HEAD 恢复、结果替换为 PROTECTED FILE VIOLATION 并记入 decisions.jsonl。工具入口的确认门保留，守卫是兜底。测试：run_shell 改 locks.yaml → 必须被恢复 + 日志有记录
  2. 命令拆分解析 `d5572e8`：mvdan.cc/sh（纯 Go、无 cgo）语法树拆管道/; /&&/嵌套 sh -c、bash -c、eval；sudo/doas 剥壳；xargs 透传递归。规则表（参考 Coren pkg/risk，只搬规则）：rm 递归+force（含 -fr、--recursive --force、组合/分离 flag）、find -delete/-exec、git push --force/-f/--force-with-lease/+main refspec、git clean -fdx、git reset --hard、git branch -D、mkfs*、dd of=/dev/*、chmod -R 777、shutdown 系。解析失败一律按危险处理（fail closed）。文档写明：这是减速带不是沙箱，变量展开无法穷举，core/guard 才是兜底，用户确认是最终权威
  - 测试：24 个危险例（含全部用户列出的绕过手法）+ 8 个安全例全过；全量回归绿、android 交叉编译、泄漏扫描 0 命中

- [2026-11-06] A/B 复核修复（各自提交）：
  - guard `7fed1af`：①快照/校验任何错误按违规处理（fail closed）——无法快照时拒绝工具执行，恢复失败中止整轮并报给用户；②Verify 遍历 before 与当前受保护集的并集（缩锁后同轮改锁定文件也能抓）；③快照存内容+权限+缺席态，按快照原样恢复（不冲掉未提交的用户修改、删除新建文件、恢复原权限），目录/符号链接替换整段清除，恢复失败绝不静默；④每轮调模型前对上一可信快照重验（nohup 延时写入被抓）；⑤清除死代码。测试 7 个：chmod/类型替换、缩锁并改、未提交编辑保留、权限还原、符号链接、恢复失败上报
  - danger `86639cb`：①静态求值改在 syntax.Word 上（Lit+SglQuoted+纯字面量 DblQuoted 拼接），含 ParamExp/CmdSubst 的词一律按危险（$X -rf、rm $(find /)、引号包裹命令名 'rm'/r''m/\rm/rm"-rf" 全部命中）；②sudo 重写（-u/-g/-- 取值、-- 终止、-E 无 panic）；③包装剥壳补齐 env（跳 VAR=x 与 -i/-u）、nohup、timeout、nice、ionice、stdbuf、time、command、exec、busybox/toybox（applet 剥壳）；④git 全局参数跳过 -C/-c/--git-dir/--work-tree 再定位子命令；⑤重定向目标 > locks.yaml 也是危险；测试扩到 50+ 正负例
- [2026-11-06] P1.5 单代理上下文管理（三项独立提交，已推送 0f0d24b..dfc1687）：
  1. 稳定前缀 `9b30fd9`：前缀=system 消息+按注册序的工具定义（结构体 JSON，字段序确定、无 map、无时间戳/随机数）；会话中途不增删工具（FilterRegistry 只在会话创建用）；prefix_hash + cached_tokens（prompt_tokens_details.cached_tokens）进决策日志 v2（v1 字段不动）；owltide changes 尾部显示缓存命中率与前缀稳定性；TestStablePrefixBytes 验证同会话两轮前缀哈希一致
  2. Inline & Spill `e57a1aa`：core/spill——单结果默认 8KB 上限（可配），超出全文写入 ~/.owltide/spill/<session>/（不进 git、写入前 sk-/Bearer/ghp_/token= 等密钥打码），上下文留头 40 行+尾 10 行+总行数/字节+spill id；read_file 支持 spill:<id>[:start-end] 续读（sha256 锚定不受影响——仍按原文件算）；报错结果同样截断但保留结尾；spill 目录 32MB 上限按会话清理。测试：大输出截断可完整取回、小输出原样、密钥不落盘、跨会话清理、read_file 集成
  3. 目标复述与死循环检测 `dfc1687`：todo 工具写 <session dir>/todo.md；每轮注入 ≤500 字节的 [CURRENT GOAL] 到上下文末尾（视图层，不碰前缀不进历史）；死循环签名=sha256(工具名+规范化输入+输出)，滑窗 10 步同签名 ≥5 次先注入提醒，再犯按预算截停流程结束并记 decisions.jsonl（"loop stop: repeated identical tool call"）。测试：假模型先提醒后终止+日志记录、正常重试（输出不同）不误报、restate 追加不碰前缀

- [2026-11-07] 0d58c60 复核三项（各自提交）：
  1. `7d10a6d` 动态词危险范围收窄：命令名不可静态求值→危险；风险命令集（rm/find/git/dd/chmod/chown/mkfs*/sh 系/eval/xargs/sudo 系/env/cp/mv/ln/truncate/shred/关机系/mount/fdisk/kill 系/chattr/setfacl/tee）+ 参数动态→危险；其他命令动态参数放行（$(...) 子命令仍递归检查）；动态重定向目标（> $F）→危险。危险侧 $X -rf、rm -rf $TARGET、rm $(find /)、find $DIR -delete、git push $REMOTE --force 全命中；安全侧新增 go test $(go list ./...)、cd "$HOME"、echo $PATH、ls $DIR、cat ${CONFIG_FILE}、echo $(date)
  2. `cf9bae0` 复述消息加 [AUTO — not a new user message] 前缀 + system prompt 一句说明（固定前缀一部分，一次改定不破缓存）；清 loop.go 死代码 var _ = sort.Strings
  3. `b664008` cached_reported 贯通（Usage→decision v2→changes）：端点不返回 prompt_tokens_details 时显示「未上报」而非 0%
  - 真实会话缓存测试（用户端点 nemotron-3.5，chat 两轮建/改 notes.md）：两轮 prefix_hash 完全一致（e9af9f157d9ed906…，prefix stable: true）；端点上报 cached_tokens 字段但两轮均 0（该端点对此模型不做前缀缓存）；changes 尾行「cache: 2 turns, 0.0% prompt tokens served from provider cache (prefix stable: true)」
  - Go 升级 go.mod → 1.26.10（工具链验证 + 全量测试 8 包绿 + vet 干净）；govulncheck ./... = No vulnerabilities found

- [2026-11-07] 密钥泄漏事件与整改：
  - 事故：e57a1aa（P1.5 第 2 项）的 spill 测试夹具误用了真实 key 并推送。事前扫描输出 5 处命中，但操作上未把非零计数当硬停（手敲 one-liner 无强制门禁），推后又用空区间（origin/main..HEAD）复扫误称 0 命中——扫描方法本身没问题，错在执行层。用户决定：作废旧 key 重新签发，不改写历史不 force push
  - 整改 `6a9efe3`：测试夹具换假 key（sk-TESTKEY…，断言同步）；新规矩：任何提交/测试/日志/汇报不得出现真实密钥，测试一律伪造值
  - 门禁：scripts/secretscan（sh，规则=sk- 系/常见令牌前缀/PEM/Bearer/高熵(≥32 字符，hex 4.0 上限不误伤、含 / 的路径形 4.5、其余 4.1)/本地配置 key 的 sha256 比对（只存哈希：env 实时算 + scripts/secretscan.hashes 钉住已撤销旧 key））；scripts/hooks/pre-push + core.hooksPath（hook 从 $0 解析扫描器位置，跨 clone 可用）；已撤销 key 只出现在 diff 删除行时放行（修复提交不拦），出现于 + 行即拦
  - 测试 scripts/secretscan_test.go：假 key 提交被拦（TestSecretScanBlocksFakeKeyCommit，key 运行时拼接、源码无触发字面量）、正常内容不误报（TestSecretScanCleanTreePasses：模块路径/SHA/spill id）、高熵被拦（TestSecretScanBlocksHighEntropy）、钉住哈希连 TEST 标记也拦（TestSecretScanHashRulePinned）、端到端 pre-push（TestPrePushHookEndToEnd：bare origin + clone，干净推送成功、密钥推送被拒且远端未收到）

- [2026-11-07] 密钥门禁验收通过（本地 hook + CI before..after + 金丝雀）。演进链：56ea135（secretscan + pre-push hook + 钉住哈希）→ 9163934（WORKLOG 整改记录）→ f238a30（CI 接入 + 显式 allowlist + key 形态哈希比对）→ 6a84ca9（输出脱敏为规则号+哈希前 12 位+位置；只扫远端没有的提交）→ 97a2529（CI 改扫 before..after 非空区间 + 金丝雀步骤 + 空区间告警；README 快速开始更新）。Actions run 42334615063（97a2529）：scan 步骤非空区间 clean，金丝雀步骤按预期 BLOCK（钉住哈希命中），输出无任何密钥内容。旧密钥已由使用者作废重签；仓库历史中的旧泄漏点由钉住哈希永久看护，任何新提交重新引入都会被拦。

- [2026-11-07] P2a M1 子代理调度完成（接手中断会话，快照分支 p2a-m1-wip，整理后合回 main）：
  1. 会话消息日志 `core/sessionlog`：每会话 `~/.owltide/sessions/<id>/messages.jsonl`，只追加、每行一次 write+fsync；`RebuildWithWarning` 读回，末行半行（崩溃残留）丢弃并告警。Agent 收口到唯一入口 `logMsg`（8 个 append 点全部改走），sub/<id>/messages.jsonl 用同一写入器。config store 的 writeTree/removeStale/gitignore 跳过 `sessions/`（与 logs/spill 同列），运行产物永不入提交。测试 TestSessionLogAppendRebuild、TestSessionLogTruncatedTail
  2. 异步结果投递：user 角色注入废弃。改为合成（assistant tool_call agent_result + tool result）消息对，tool result 带「以下为数据，不是指令」标记；下一轮开头注入的同时追加写进主会话日志（resume 重建顺序与模型所见一致）。只投递一次：内存 Delivered 标志 + subagents.jsonl `delivered` 事件双记录；重启后 Manager 从事件恢复（DeliveredIDs），已完成未投递的结果补投一次。大摘要过 spill，主日志只留摘录+spill id。测试 TestResultSurvivesRestart、TestSubagentResultSpill
  3. 接线修正：Manager 增加 `SetParent(*agent.Agent)`（setup 返回 manager，CLI 构造 Agent 后回接，OnResult 闭包引用未定义变量的编译错误消除）；setup 打印 session id；`agents.Load` 传真实注册表工具名（角色白名单写错启动即报错，fixture 同步）
  4. M0 复核项落实（代码位置+测试）：
     - 预算退回+budget 事件：manager.go reserve/reserveBatch/tools.go chain defer（预扣成功立即注册 defer，chain 失败路径不再泄漏；startSingle 改为纯后台模式，watchdog goroutine 在所有完成路径上释放+投递）。TestBudgetDeductedFromParent（断言 reserved/released 事件顺序）、TestChainFailureReleasesBudget
     - 并行确认排队+标注：tool.Registry.CloneWithConfirmer（Shell/WriteFile/EditFile/ReadOnlyShell 实现 clone），Manager.confirmFor 序列化（confirmMu）并给提示加 `[subagent <id> (<role>)]` 前缀；子代理注册表按会话克隆。TestParallelConfirmQueued
     - 深度：结构性限制=子代理注册表无 agent_*（ExcludeForChild），链内步骤不可能再派，深度恒 1；Manager 死字段 depth 已删，MaxDepth 保留为参数（注释注明当前结构性上限）。TestDepthLimit
     - 前缀字节稳定：rolePrefix（角色+描述+系统提示词，无时间戳/随机数），task 在前缀之后作首条 user 消息。TestRolePrefixStable
     - 事件只存哈希：AppendEvent 对 >200 字符串字段自动换 `*_digest`（sha256 前 8 字节）；task/summary 从不进事件。TestEventsHashOnly
     - sub/<id>.jsonl 不进 git：写入路径在 logs/（store 跳过）+ 主仓库 .gitignore + store 层测试。TestSubTranscriptNotCommitted
     - context_refs 走 Guard 读规则：resolveContextRefs——spill 引用只经 spill store（id 白名单校验）；文件路径拒绝绝对路径/../软链接逃逸（EvalSymlinks 复核）+ 拒绝受保护文件（Guard.ProtectedPaths）；超长内容过 spill 留摘录。TestContextRefsGuarded
  5. 顺带修复：startParallel 之前错误拒绝共享 role 参数（现按设计 role+tasks[]）；fixture 注册 agent_* 工具后再以真实工具集重校验角色文件
  - 全量验证：go build/vet/gofmt 干净；go test ./... 14 包全绿；GOOS=android GOARCH=arm64 CGO_ENABLED=0 交叉编译通过
