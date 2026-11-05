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
- 真模型端到端：用户端点 的 nemotron-3.5-super-vl-120b-a12b 自主调 list_dir → write_file → run_shell 建 hello.go 并运行成功；decisions.jsonl 每轮有记录（含失败轮）
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

- P0 已全部完成，待用户指示 P1 方向
- 候选（来自原始需求，未排期）：`~/.owltide` 自动 git 提交、agent.upsert 自进化、记忆系统
- 遗留：首次 run 曾测过 `poolside/laguna-xs-2.1`、`deepseek-ai/deepseek-v4.1-flash` 等其他端点模型，未系统对比工具调用能力，需要时可做模型选型评测

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
- [2026-11-05] P0 收尾：`go mod tidy` 依赖提交（`a8ac633`）；真模型端到端验收通过（用户端点 nemotron-3.5，建 hello.go 并运行成功）。首次 401 是因为 key 属 用户端点 而配置指向 deepseek，改配置后通过。P0 完成，11 个本地提交。
