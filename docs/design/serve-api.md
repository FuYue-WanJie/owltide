# owltide serve — 只读 API 清单

状态：D 项交付（Web UI 后端骨架）
绑定：默认 `127.0.0.1:7877`，仅允许 loopback（`--addr` 也拒绝非 loopback，见 `isLoopbackAddr`）

## 接口

| 方法 | 路径 | 说明 | 响应 |
|------|------|------|------|
| GET | `/` | 占位页（go:embed index.html） | text/html |
| GET | `/api/sessions` | 会话列表（最新在前） | `[{id, messages, modified}]` |
| GET | `/api/sessions/{id}` | 某会话的消息（messages.jsonl 逐行 JSON；坏行跳过） | `[message...]` |
| GET | `/api/subagents` | 子代理状态快照 | `[{id, role, state, task, delivered}]` |
| GET | `/api/jobs` | 后台作业状态快照 | `[{id, state, exit_code, command, started, dropped}]` |
| GET | `/api/events` | SSE 事件流（`id: <seq>` + `data: {"seq":...,"event":...,"data":...}`；15s 心跳 `: ping`；支持 `Last-Event-ID` 补发） | text/event-stream |

## 约定与边界

- **只读**：无任何写接口（发消息、审批等等界面设计确定后另加）
- **无密钥**：serve 不加载 config 的 secret 材料；接口响应只含会话日志内容与快照函数返回的字段。测试 `TestNoSecretMaterial` 固化该不变量
- **路径安全**：session id 白名单校验（`[0-9a-zA-Z_-]`，≤64 字符），目录穿越/非法名一律 400/404；List 同样过滤非法目录名
- **SSE 发布**：进程内 `Server.Publish(event, data)`；事件带递增 `seq`（当前进程内单调递增，从 1 开始）
- **SSE 补发**：服务端保留最近 N 条事件（环形，N 暂定 1024）；客户端断线重连带 `Last-Event-ID: <seq>` 时，从 seq+1 起补发，超出环形窗口则发 `reset` 事件要求全量刷新
- **SSE 投递分级**：
  - **必达**：审批类事件（`approval_*`，来源第 6 节权限服务）——不允许走"慢客户端丢帧"，客户端缓冲满时阻塞该端点的读协程（背压），不丢事件
  - **可丢**：进度类事件（`progress`、`state` 的高频更新）——慢客户端可丢帧或只留最新一条
- **纯 Go**：仅标准库 net/http，无 cgo、无新依赖

## 事件（SSE）

| event | data | 投递等级 |
|-------|------|----------|
| `hello` | serve 启动通知（M0） | 可丢 |
| `message` | `{session}` — 新消息落盘（M0 预留，CLI 侧接线随 UI 里程碑） | 可丢 |
| `state` | 子代理/作业状态变化（M0 预留） | 可丢 |
| `approval_pending` | `{id, kind, source, payload}` — 权限服务待审批（第 6 节） | **必达** |
| `approval_resolved` | `{id, state, by}` — 已由某端处理（先到先得，后到端收到此事件） | **必达** |
| `reset` | `{}` — Last-Event-ID 超出补发窗口，要求客户端全量刷新 | 必达 |

> `approval_*` 事件在接入权限服务（P2b-M16）后启用；M0 的 serve 只有只读事件。

## 已知边界（M0）

- 独立 `serve` 进程只展示持久化的会话；活代理的 subagent/job 快照需在 chat/run 进程内接 `State.SetWiring`（随 UI 里程碑做进程内嵌 serve 或事件桥）
- 写接口（发消息、审批回答）在权限服务（P2b 第 6 节）完成后才加，且必须走权限服务，不另起审批通道
- `seq` 当前是进程内序号：serve 重启后从 1 重来，客户端以 `hello` 事件为界重置本地游标
