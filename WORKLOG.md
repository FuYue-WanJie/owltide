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

- [2026-11-06] P0+P1 推送完成：28+4 提交全部上 origin/main（fast-forward，无 force）。推送前对数澄清：之前报 16 是只数了 P1 段（a8ac633..HEAD），漏报 P0 的 11——P0 实际从未推送。未推历史经 filter-branch 洗掉 WORKLOG 里的私有端点域名（ltzy.top），终扫 sk-/ltzy/key 值 0 命中。
- [2026-11-06] P1 补充三项（各自独立提交）：
  1. 哈希锚定编辑 `1150c66`：read_file 尾部附 [sha256:...] 标记；edit_file 必填 expect_sha256，不匹配拒绝并返回最新片段（old_string 附近 ±5 行或文件头 25 行）
  2. agents/*.md 格式 `43bdeb8`：frontmatter（name/description/model/tools/budget）+ markdown body 即 system_prompt；Save/Load/Remove 全切 .md；Def.FilterRegistry 是白名单强制点（子代理注册表只含白名单工具，P2 调度接入）；校验新增 description 必填
  3. 危险命令强制确认 `178d8b3`：run_shell 检测 rm -rf（含组合 flag）/git push --force/-f/git reset --hard/mkfs/dd of=/dev/*/chmod -R 777/shutdown 系；write_file/edit_file 检测 locks.yaml；config:budget 沿用永久锁拒绝。Registry.SetConfirmer 注入用户确认回调（CLI stdin y/N），模型无法自证放行；无确认器（非交互）一律拒绝；拒绝经 tool error 通道进 decisions.jsonl（TestDangerousCommandLogged 验证）
  - 测试：agents roundtrip/frontmatter 解析/白名单过滤、danger 模式正负例、locks.yaml 门、agent 决策日志；全绿 + android 交叉编译 + gofmt/vet 干净

- [2026-11-06] 危险命令确认两处漏洞修复（用户复核）：
  1. 受保护文件事务守卫 `905cfb1`：新包 core/guard——每次工具调用前后对受保护文件（locks.yaml、config.yaml、全部锁定项路径，动态含新锁）做哈希快照/比对；run_shell echo/sed/cp/tee 改锁文件会被检出、从 config store HEAD 恢复、结果替换为 PROTECTED FILE VIOLATION 并记入 decisions.jsonl。工具入口的确认门保留，守卫是兜底。测试：run_shell 改 locks.yaml → 必须被恢复 + 日志有记录
  2. 命令拆分解析 `d5572e8`：mvdan.cc/sh（纯 Go、无 cgo）语法树拆管道/; /&&/嵌套 sh -c、bash -c、eval；sudo/doas 剥壳；xargs 透传递归。规则表（参考 Coren pkg/risk，只搬规则）：rm 递归+force（含 -fr、--recursive --force、组合/分离 flag）、find -delete/-exec、git push --force/-f/--force-with-lease/+main refspec、git clean -fdx、git reset --hard、git branch -D、mkfs*、dd of=/dev/*、chmod -R 777、shutdown 系。解析失败一律按危险处理（fail closed）。文档写明：这是减速带不是沙箱，变量展开无法穷举，core/guard 才是兜底，用户确认是最终权威
  - 测试：24 个危险例（含全部用户列出的绕过手法）+ 8 个安全例全过；全量回归绿、android 交叉编译、泄漏扫描 0 命中
