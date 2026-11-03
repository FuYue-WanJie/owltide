<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/wordmark-dark.svg">
    <img src="assets/wordmark-light.svg" alt="OwlTide" width="360">
  </picture>
</p>

<p align="center">一个长期在线、会自己进化的个人 Agent。写代码，也陪你聊天。</p>

---

> 🚧 设计阶段，尚未开始编码。前身为 [coren](https://github.com/FuYue-WanJie/coren)，此次为完全重写。

## 它是什么

- **主代理 + 子代理 + 环境 + 工具**：主代理规划调度，子代理各自独立上下文干活。
- **自进化**：用专用工具修改自己的子代理、工具、Skills 和记忆；每次改动自动进 git，失败自动回滚。
- **无人值守友好**：默认按自动通行设计，护栏靠校验、预算和回滚，而不是靠人盯。
- **多入口**：CLI、WebUI、Android 客户端、QQ（OneBot）。
- **薄核心**：扩展直接复用 MCP 和 Skills，不自造插件系统。

## 跑在哪里

- **Android 手机**：Go 核心以原生 arm64 程序运行，由 Kotlin 前台服务拉起；只有执行命令时才进入 proot 容器。
- **吃资源的任务**：交给 InstaBox 远程机器或 GitHub Actions。
- **可选备机**：Hack Club Nest。

## 仓库结构（规划）

```
core/      Go 核心
android/   Kotlin 客户端
web/       WebUI
docs/      文档
assets/    品牌资源
```

## 文档

- [架构草案](docs/ARCHITECTURE.md)
