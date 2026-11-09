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
| GET | `/api/events` | SSE 事件流（`data: {"event":..., "data":...}`；15s 心跳 `: ping`） | text/event-stream |

## 约定与边界

- **只读**：无任何写接口（发消息、审批等等界面设计确定后另加）
- **无密钥**：serve 不加载 config 的 secret 材料；接口响应只含会话日志内容与快照函数返回的字段。测试 `TestNoSecretMaterial` 固化该不变量
- **路径安全**：session id 白名单校验（`[0-9a-zA-Z_-]`，≤64 字符），目录穿越/非法名一律 400/404；List 同样过滤非法目录名
- **SSE 发布**：进程内 `Server.Publish(event, data)`；慢客户端丢帧不断流
- **纯 Go**：仅标准库 net/http，无 cgo、无新依赖

## 事件（SSE）

| event | data |
|-------|------|
| `hello` | serve 启动通知（M0） |
| `message` | `{session}` — 新消息落盘（M0 预留，CLI 侧接线随 UI 里程碑） |
| `state` | 子代理/作业状态变化（M0 预留） |

## 已知边界（M0）

- 独立 `serve` 进程只展示持久化的会话；活代理的 subagent/job 快照需在 chat/run 进程内接 `State.SetWiring`（随 UI 里程碑做进程内嵌 serve 或事件桥）
