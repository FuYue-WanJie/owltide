# OwlTide 架构草案（v0）

> 状态：草案，随时推翻。目标是**薄核心 + 可自我修改的配置层**，而不是自造一套插件系统。

## 1. 定位

一个长期在线的个人 Agent：既能写代码，也能当日常帮手和群聊里的「赛博群友」。

形态：**主代理 + 子代理 + 环境 + 工具**。

参考：DeepSeek Harness、ZCode、StepCode、OpenCode、OpenClaw、Hermes Agent（自进化）。

## 2. 分层

```
┌──────────── 入口 Channels ────────────┐
│  WebUI   CLI   QQ（OneBot 协议）  …   │   只负责收发消息，不含业务逻辑
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
  channels/    入口配置（QQ 群、触发词…）
  config.yaml  全局配置
```

- 整个目录是一个本地 git 仓库，**每次改动自动提交**。
- 核心代码不在这里，Agent 改不到自己的 Go 代码。

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

## 5. 自动通行（无人值守）下的护栏

默认假设用户开着自动通行，护栏不能靠人盯：

1. **Schema 校验**：配置写不对就拒绝，不落盘。
2. **保护区**：`config.yaml` 里的安全项（预算、保护区本身）只能由人改。
3. **健康检查 + 自动回滚**：改完跑一次自检，挂了就回到上一个提交。
4. **预算**：每个子代理有 Token / 次数 / 时长上限。
5. **工具隔离**：自写工具一律子进程运行，崩了不带崩核心。
6. **变更日报**：每天汇总 Agent 改了自己什么，人有空时看一眼即可。

## 6. 运行在哪里

### 主机：手机优先

- 主力跑在 Android 手机上（已关闭后台子进程限制，应用自带保活）。
- Hack Club Nest 作为可选备机：手机关机、断网、发热时再把同一个二进制部署上去。

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

## 7. QQ 入口

- 走 OneBot v11/v12 协议，对接 NapCat 等实现。
- 群聊模式：被 @ 或命中触发词才回复，有冷却与每日上限。
- 注意：非官方 QQ 协议有封号风险，建议用小号。

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
8. WebUI，最后 QQ。
