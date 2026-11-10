# OwlTide 架构草案（v0）

> 状态：草案，随时推翻。目标是**薄核心 + 可自我修改的配置层**，而不是自造一套插件系统。

## 1. 定位

一个长期在线的个人 Agent：既能写代码，也能当日常帮手和群聊里的「赛博群友」。

形态：**主代理 + 子代理 + 环境 + 工具**。

参考：DeepSeek Harness、ZCode、StepCode、OpenCode、OpenClaw、Hermes Agent（自进化）。

## 2. 分层

```
┌──────────── 入口 Channels ────────────┐
│  WebUI   CLI   社交入口（Channel 插件）  │   只负责收发消息，不含业务逻辑
└──────────────┬────────────────────────┘
               │ Session API（统一会话接口）
┌──────────────▼────────────────────────┐
│ Core：Agent 循环                       │
│  · 主代理：理解 → 规划 → 调度          │
│  · 子代理：按配置启动，独立上下文      │
│  · 模型适配：OpenAI 兼容 / DeepSeek …  │
│  · 上下文与 Token 管理                 │
└──────────────┬────────────────────────┘
               │
┌──────────────▼────────────────────────┐
│ 能力层                                 │
│  · 内置工具（文件、Shell、搜索…）       │
│  · MCP 服务器（外部工具，子进程）       │
│  · Skills（Markdown 说明书）           │
│  · 记忆（Markdown 文件）               │
└──────────────┬────────────────────────┘
               │
┌──────────────▼────────────────────────┐
│ 环境 Environment                       │
│  本地 proot / InstaBox / GitHub Actions │
└────────────────────────────────────────┘
```

## 3. 一切可改的东西都是文件

```
~/.owltide/
  agents/      子代理定义（YAML：模型、提示词、可用工具、预算）
  tools/       自写工具（MCP 服务器，子进程运行）
  skills/      Skills（Markdown）
  memory/      记忆（Markdown）
  channels/    入口配置（社交平台群、触发词…）
  config.yaml  全局配置
```

- 整个目录是一个本地 git 仓库，**每次改动自动提交**。
- 核心代码不在这里，Agent 改不到自己的 Go 代码。

### 3.1 P1 目录布局（实现定稿）

```
~/.owltide/
  agents/      子代理定义（YAML：模型、提示词、可用工具、预算）
  skills/      Skills（Markdown，P1 只管写文件）
  memory/      记忆（Markdown，P1 只管写文件）
  channels/    入口配置（社交平台群、触发词…，P1 只留目录）
  config.yaml  全局配置
  locks.yaml   锁定名单（见 5.1）
  logs/        决策日志（.gitignore，不入 git）
  .git/        自动提交仓库
```

`tools/`（自写 MCP 工具）留到 P2。

## 4. 自进化：专用配置工具

不给 Agent 随手改文件，而是给它几把专用工具：

| 工具 | 作用 |
| --- | --- |
| `agent.upsert` / `agent.remove` | 新增或修改子代理 |
| `tool.create` / `tool.update` | 写或改 MCP 工具 |
| `skill.write` | 写或改 Skill |
| `memory.write` | 写记忆 |
| `config.patch` | 改全局或入口配置 |
| `history.rollback` | 回滚到任意一次提交 |

每把工具都：**校验 schema → 写入 → git 提交 → 热重载 → 健康检查**，失败自动回滚。

### 4.1 P1 实现定稿

- P1 落地六把：`agent.upsert`、`agent.remove`、`skill.write`、`memory.write`、`config.patch`、`history.rollback`（`tool.create/tool.update` 留 P2）。
- **统一流程（store.Transaction）**：校验 schema → 检查锁 → 写文件 → git 提交 → 热重载 → 健康检查；任一步失败自动 revert 到上一个提交，并把失败原因作为工具结果返回给模型。
- **自动 git 选型**：`core/store` 手写极简提交引擎（SHA-1 + zlib 对象、tree/commit、refs、log），零第三方依赖——Android 未必有系统 git，go-git 违背依赖纪律。对象格式与标准 git 兼容，人可直接用 `git log` 查看。核心代码不依赖 cgo。
- 提交信息记录：who（agent|user）、调用的工具、会话 ID。

## 4.2 子代理定义（P1）

`agents/*.yaml`，schema：

```yaml
name: reviewer            # 必填，^[a-z][a-z0-9-]*$，目录内唯一
model: <模型名>            # 必填
base_url: <可选，缺省用全局>
system_prompt: <必填，非空>
tools: [read_file, ...]   # 必填数组，只能是已注册工具名
budget:                   # 必填，各项 > 0
  max_turns: 10
  max_tokens: 100000
  tool_timeout_sec: 30
```

P1 只管增删改和加载（启动时读入并校验）；调度留 P2。

## 5. 自动通行（无人值守）下的护栏

默认假设用户开着自动通行，护栏不能靠人盯：

1. **Schema 校验**：配置写不对就拒绝，不落盘。
2. **保护区**：`config.yaml` 里的安全项（预算、保护区本身）只能由人改。
3. **健康检查 + 自动回滚**：改完跑一次自检，挂了就回到上一个提交。
4. **预算**：每个子代理有 Token / 次数 / 时长上限。
5. **工具隔离**：自写工具一律子进程运行，崩了不带崩核心。
6. **变更日报**：每天汇总 Agent 改了自己什么，人有空时看一眼即可。

### 5.1 锁定配置（P1 实现定稿）

- 任何配置项、子代理、Skill 都可标为 `locked`，登记在 `~/.owltide/locks.yaml`。
- 锁住的东西 Agent 照常**能用**，但自进化工具**改不了、删不了**——包括开着自动通行也一样。
- **锁名单本身和预算上限永远锁定**：`locks.yaml` 与 `config:budget` 不接受来自 Agent 的修改，也不出现在可解锁范围之外。
- 只有**人**能解锁：CLI `owltide lock <id>` / `owltide unlock <id>`，不注册为 Agent 工具。
- `history.rollback` 绕不开锁：回滚生成的变更先过锁检查，命中锁定项的改动被剔除后再提交。
- 锁 id 形如 `config:budget`、`agent:reviewer`、`skill:xxx.md`、`memory:xxx.md`。

## 5.2 变更日报与决策日志（P1）

- `owltide changes [--since 24h]`：从 store 的提交历史列出谁（agent/user）在何时用哪把工具改了什么（每提交的路径清单）。
- 自进化工具的每次调用（含被拒绝的）写入 `decisions.jsonl`：v1 字段不改不删，经 `tools_called` 自然承载；若未来需要专属字段，升 v2 并在本文件记录迁移。

## 6. 运行在哪里

### 主机：手机优先

- 主力跑在 Android 手机上（已关闭后台子进程限制，应用自带保活）。
- 不绑定备机：同一个二进制可部署到任何支持的平台（电脑、Linux 服务器等），手机不便时换个地方跑即可。

### Android 上的形态：核心原生，环境容器

```
Kotlin 应用
 ├─ 前台服务 ──拉起──▶ libowltide.so（Go 编译的 arm64 可执行文件）
 └─ 界面 ◀──本地 WebSocket──▶ Session API
                                │
                                ├─ 文件读写、搜索、编辑：Go 直接操作（不启动容器）
                                └─ 执行命令：按需拉起 proot 容器
```

- Go 核心交叉编译为 `android/arm64`，放进 `jniLibs/arm64-v8a/libowltide.so`。Android 10 起只有原生库目录允许执行文件，这是绕过 W^X 限制的标准做法。
- 原生运行，没有模拟层；本地 WebSocket 往返不到 1 毫秒，相对模型延迟可忽略。
- proot 通过 ptrace 拦截系统调用，大量文件 I/O 时明显变慢，所以**核心不进容器**，只有跑命令时才进。
- 容器 rootfs 只是磁盘上的目录，核心可直接读写。注意：容器内的绝对路径软链接要相对 rootfs 解析；权限是 proot 伪造的，不要从外部改。
- 前台服务 + 电池优化白名单，防止被系统杀后台。

### 三种执行环境

| 环境 | 适合 | 说明 |
| --- | --- | --- |
| 本地 proot | 日常命令、小脚本 | 零成本，离线可用 |
| InstaBox | 编译等吃资源的交互任务 | 免费版 1 台 4 vCPU / 8 GiB / 20 GiB，文件持久，exec API，闲置 30 分钟自动暂停；内测期，免费不保证长期 |
| GitHub Actions | 打包、发版等批处理 | 公开仓库免费，单任务最长 6 小时，产物以 artifact 取回 |

核心按任务自动选择环境，也可以在子代理配置里指定。

## 7. 社交平台入口（Channel 插件）

- 社交平台通过 Channel 插件接入，每个平台一个插件，核心不感知平台细节。
- 优先支持各平台的官方机器人 API。
- 另提供通用 OneBot v11 适配，由用户自行配置协议端。
- 非官方协议端可能违反平台条款并导致封号，风险由使用者自行承担；本项目不提供、不推荐任何具体实现，也不引用其代码。
- 群聊模式：被 @ 或命中触发词才回复，有冷却与每日上限。
- 社交入口一律按只读处理：不能从群里触发写文件或 shell（见 `docs/design/p2b.md` 第 6 节权限服务）。

## 8. 仓库结构

单仓库，前期不拆：

```
owltide/
  core/      Go 核心（Agent 循环、工具、Session API）
  android/   Kotlin 客户端（前台服务 + 界面）
  web/       WebUI
  docs/      文档
  assets/    品牌资源
```

CI：先 `GOOS=android GOARCH=arm64` 编译核心，放入 `android/app/src/main/jniLibs/arm64-v8a/`，再打 APK。客户端需要独立发版时再拆仓库。

## 9. 第一步做什么

1. Core：单代理循环 + OpenAI 兼容模型适配 + 文件/Shell 工具。
2. CLI 入口。
3. `~/.owltide` 目录 + git 自动提交 + `agent.upsert`。
4. 子代理调度。
5. MCP、Skills、记忆。
6. Android 客户端（前台服务 + 界面）。
7. 远程环境：InstaBox、GitHub Actions。
8. WebUI 与社交入口（Channel 插件）。
