<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark-light.svg" alt="OwlTide" width="360">
  </picture>
</p>

<p align="center">一个长期在线、会自己进化的个人 Agent。写代码，也陪你聊天。</p>

---

> 🚧 **P1.5 已完成**：单代理 CLI + 可自我修改的配置层 + 护栏。前身为 [coren](https://github.com/FuYue-WanJie/coren)，此次为完全重写。

## 它是什么

设计目标与当前状态：

- ✅ **已实现**：单代理循环（理解 → 调工具 → 读结果 → 继续/回答），预算与轮数上限，流式输出。
- ✅ **已实现**：自进化工具（agent_upsert / agent_remove / skill_write / memory_write / config_patch / history_rollback），配置层自动 git 提交，失败自动回滚。
- ✅ **已实现**：锁定配置（保护区）+ 变更日报 + 决策日志。
- 🔜 **规划中（P2a）**：子代理调度——主代理规划调度，子代理各自独立上下文干活。
- 🔜 **规划中（P2b）**：上下文压缩、跨会话记忆。
- 🔜 **规划中**：WebUI、Android 客户端、QQ（OneBot）入口。
- 🔜 **规划中**：长期在线（守护进程化、多入口常驻）。

## 现在能干什么（P0–P1.5）

```bash
go build -o owltide ./cmd/owltide

# 密钥只走环境变量，不落盘；用 read 输入，不会出现在命令行和历史里
read -rs OWLTIDE_API_KEY && export OWLTIDE_API_KEY

owltide init                # 生成 ~/.owltide 配置；编辑 config.yaml 填入
                            # model.base_url（如 https://api.example.com/v1）
                            # 与 model.name
owltide run "在当前目录建一个 hello.go 并运行"
owltide chat                # 多轮交互
owltide lock config:budget  # 把配置项锁进保护区（只有人能解锁）
owltide unlock config:budget
owltide changes --since=24h # 看 Agent 改了自己什么（含模型缓存命中率）
```

- **工具**：read_file（带 sha256 锚定的 edit_file）、write_file、list_dir、run_shell（危险命令需终端确认）、todo（目标复述与死循环检测）。
- **自进化工具**：统一走「校验 → 锁检查 → 写入 → git 提交 → 热重载 → 健康检查 → 失败回滚」。
- **通行策略**：普通操作自动通行；危险命令（rm -rf、git push --force、改锁文件等）和受保护配置需要人确认或解锁——模型无法自证放行。
- **护栏**：配置层是一个 git 兼容的自动提交仓库（纯 Go 实现，Android 可用，`git log` 可读）；每次工具调用前后对受保护文件做哈希快照，越界修改立即恢复并记入决策日志；大输出自动溢出到磁盘（上下文只留摘要，可按行续读）。
- **测试**：`go test ./...`，模型用假服务器模拟，不烧 Token。

## 跑在哪里

核心是一个跨平台的 Go 程序，Go 能编译到的地方都能跑：电脑、服务器、Android 手机（`GOOS=android GOARCH=arm64 CGO_ENABLED=0`）。

- **Android**（规划中）：核心以原生 arm64 程序运行，由 Kotlin 前台服务拉起；只有执行命令时才进入 proot 容器。
- **吃资源的任务**：交给远程环境（如 InstaBox）或 GitHub Actions。

## 仓库结构

```
core/      Go 核心（agent/model/tool/agents/evolve/store/guard/spill/decision/config/platform）
cmd/       CLI 入口
scripts/   secretscan（推送前密钥扫描 + pre-push hook）
.github/   secretscan CI（push / PR 均扫描待推提交）
docs/      文档
assets/    品牌资源
WORKLOG.md 工作日志（进展、决策、复盘）
```

## 文档

- [架构草案](docs/ARCHITECTURE.md)（含锁定配置、自进化工具、预算与变更日报的定稿设计）

## 安全

- 密钥只走环境变量，不进配置文件、不进仓库；`scripts/secretscan` 在每次 push（本地 hook + CI）扫描**远端还没有的提交**的全部内容，命中即拒，输出只含规则号、命中哈希前 12 位与位置。
- 任何配置项、子代理、Skill 都可锁进保护区：Agent 照常能用，但改不了删不了，回滚也绕不开；只有人能解锁（`owltide lock/unlock`）。

## 路线

- **P2a**：子代理调度（复用 P1.5 的上下文管理与 agents/*.md 白名单）
- **P2b**：上下文压缩、跨会话记忆
- **之后**：MCP 工具、WebUI、Android 客户端、QQ 入口

## 许可证

待定。
