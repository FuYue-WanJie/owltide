# P2b M0 设计文档

状态：草案（M0，只设计不实现）
日期：2026-11-09
前置：P2a（sessionlog / decisions / subagent / spill / evolve / jobctl）已上线

---

## 1. Goal / Plan / Todo 文件格式与生命周期

### 1.1 数据结构

三种文件都是 Markdown，frontmatter + 正文，沿用 agents/*.md 的渲染惯例：

```markdown
---
kind: goal | plan | todo      # 文件类型
id: g-20261109-a1f3           # 短 id，newID() 同源
session: 20261109-143230      # 创建会话
status: active | done | blocked | archived
created: RFC3339
updated: RFC3339
---

（正文）
```

- **goal.md**：目标级。正文 = 目标陈述 + 验收标准列表（每条 `- [ ]`，可勾选）+ 关联 plan id 列表
- **plan.md**：计划级。正文 = 步骤列表（有序），每步一个短句 + 可选关联 todo id；头部 `goal:` 字段回指
- **todo.md**：执行级。正文 = 勾选列表；头部 `plan:` 字段回指。现有 `TodoTool`（agent.TodoTool，写 todo.md 单文件）升级为此格式，向后兼容：无 frontmatter 的旧 todo.md 首次写入时补齐

层次关系：goal 1—N plan 1—N todo，全部用 id 字段回指，不做物理嵌套。

### 1.2 写入位置

- 现有 store（config store，`~/.owltide/` 下自进化仓库）新增 `goals/`、`plans/` 子目录；todo 保持 todo.md 原位（spill.Dir 下，会话级）
- goal/plan 属跨会话资产（进 store，受 evolve 的 auto-commit 保护与 `owltide changes` 报告）；todo 属会话工作区（不进 store）
- 会话锁内写入（与 sessionlog 同一 LockSession 约束），无独立锁

### 1.3 生命周期

```
goal: active → done | blocked → archived
plan: active → done（全 todo done 才允许）| archived
todo: 无状态字段，勾选即完成；全勾 → plan 允许置 done
```

- 状态迁移只通过工具（goal_update / plan_update），不允许模型直接改 frontmatter（guard 校验 frontmatter 不可被 run_shell 触碰——已列入 protected files）
- blocked 必须带原因行（正文首行 `blocked: <原因>`），对应 AGENTS.md 停下报告语义
- 新会话启动时 agent 恢复 active goal/plan 的 id 与验收标准到系统注入（复用 [AUTO — not a new user message] 通道，见 config.System 既有语义）

### 1.4 与现有机制的关系

- **sessionlog**：goal/plan 的状态迁移作为 decision 事件记录（decisions.jsonl 已有事件管道），正文变更不进 sessionlog
- **decisions**：迁移事件 `{event: "goal_update", id, from, to, reason}` 
- **subagent**：agent_start 的 task 可引用 `goal:<id>` / `plan:<id>`，manager 的 withContextRefs 把文件正文注入子代理任务前缀（沿用 context_refs 机制，不新增通道）
- **evolve**：goals/plans 是普通 store 文件，受现有 auto-commit 与 ProtectBuiltin 无关（用户可编辑，guard 只锁 frontmatter）

### 1.5 里程碑拆分

- **P2b-M1**：格式 + 读写工具（goal_create/goal_update/plan_create/plan_update，todo 兼容升级）+ guard 保护 + 测试
- **P2b-M2**：会话恢复注入 + context_refs 引用 + decisions 事件 + 测试
- **P2b-M3**：`owltide goals` 列表命令 + store 报告集成

### 1.6 已定的问题（2026-11-10）

1. **是。** todo 保持会话级（不进 store）。
2. **否。** blocked → active 必须用户确认——blocked 本身就是停下报告语义，恢复是用户的决定。
3. **B。** 文件名 `<date>-<slug>.md`（如 `goals/20261110-add-tests.md`），id 进 frontmatter，跨文件引用一律用 id。

---

## 2. 独立上下文的验收者（verifier）

### 2.1 数据结构

新内置角色 `verifier`（readonly，进 BuiltinRoles，builtinVersion 5）：

```go
{
    Name: "verifier",
    Readonly: true,
    SystemPrompt: "You are the OwlTide verifier. You receive a claim and the
        evidence refs. Re-verify each claim independently: run the read-only
        checks yourself, never trust the claimant's summary. Answer with a
        structured verdict:\nverdict: <confirmed|partial|refuted>\nchecks:\n-
        <what you ran and saw>\ngaps:\n- <what you could not verify>",
    Tools:  ["read_file", "list_dir", "run_shell"],  // 与 review 同级
    Budget: Budget{MaxTurns: 10, MaxTokens: 100000, ToolTimeoutSec: 30},
}
```

关键约束：**verifier 与被验收者不得同源**——router 模式下，同一 plan 的验收委派禁止复用执行该 plan 的会话上下文（新 agent_start 即新上下文，天然满足；禁止向 verifier 传执行者的 summary 作为唯一证据，context_refs 只允许指向原始文件/命令输出）。

### 2.2 写入位置

- 角色定义：~/.owltide/agents/verifier.md（SeedBuiltins 播种，既有 hash 升级机制）
- 验收结果：普通 sub-agent 交付（deliver → 主日志），另加审计事件 `{event: "verified", plan|goal, verdict, sub: <id>}` 到 subagents.jsonl

### 2.3 与现有机制的关系

- **sessionlog / subagent**：完全走现有 agent_start → runSession → deliver 管道，verifier 只是角色；无新代码路径
- **decisions**：verified 事件供审计对账（可与 goal 状态迁移联动：plan 置 done 前若要求 confirmed verdict 则发软提示——已定问题 5：M0 软提示，M5 再议硬门槛）
- **jobs**：verifier 可用 run_shell（只读 gate），可用 job_output 读取执行者后台作业的残留输出作为证据

### 2.4 里程碑拆分

- **P2b-M4**：verifier 角色 + 描述/schema 改动 + 测试（角色形状、只读 gate 复用）
- **P2b-M5**：verified 审计事件 + plan→done 联动校验（若问题 5 确认）+ 测试

### 2.5 已定的问题（2026-11-10）

4. **是。** verifier 允许 parallel，计入 `MaxParallel` 与预算预扣（与现有 parallel 语义一致）。
5. **否。** M0 仅软提示（plan 置 done 无 confirmed verdict 时打提示），M5 再议硬门槛。
6. **是。** verifier 保持只读 gate。例外：config 里用户列出的验收命令（如 `go test ./...`）允许执行——精确匹配 argv（逐参数比对，不做前缀/子串匹配），模型不能增删该列表，列表变更走 evolve 的用户确认通道。

---

## 3. 上下文压缩

### 3.1 数据结构

两层压缩，均落盘可恢复（与 spill 同哲学）：

- **Turn 级（已有）**：spill.Store 已把超长工具输出落盘、主上下文留摘录 + spill 引用；P2b 不动
- **会话级（新增 compaction）**：当主上下文 token 数超过阈值（config 新字段 `budget.compact_at`，默认 0.7，**分母取自第 5 节解析出的 `context_tokens`**，不再用 `budget.max_tokens`），把最旧的已投递子代理结果对 + 已完成 turn 的工具记录折叠为一条 summary 消息

```go
// core/compaction/compaction.go
type Record struct {
    At        time.Time `json:"ts"`
    Span      []int     `json:"span"`       // 被折叠的消息序号区间（sessionlog 行号）
    Summary   string    `json:"summary"`    // 折叠摘要（模型生成或模板）
    Kept      int       `json:"kept"`       // 保留的最近消息数
}
```

- 折叠摘要本身写入 `~/.owltide/compaction/<session>.jsonl`（一行一 Record），主日志不动——**sessionlog 仍是唯一事实源**，压缩只影响内存中的请求前缀
- 摘要生成策略：M0 用模板拼接（每被折叠 turn：user 首句 + 最终 reply 首句 + 工具调用名列表），不烧 token；模型自摘要暂缓（原 M8，见下）

### 3.2 写入位置

- compaction 记录：~/.owltide/compaction/<session>.jsonl
- 阈值配置：config.yaml `budget.compact_at`（比例，0 = 关闭；默认 0.7，分母 = 第 5 节 `context_tokens`）
- 恢复：--resume 时按 compaction 记录重建折叠前缀（sessionlog 全量重建后应用 Record）

### 3.3 与现有机制的关系

- **sessionlog**：只读其内容，不修改；重建顺序 = sessionlog 全量 → 应用 compaction 折叠 → 恢复出的历史与压缩时内存一致
- **spill**：工具大输出已在 spill（摘录+引用），compaction 折叠时引用保留（折叠消息里保留 spill ref 列表，模型仍可 spill read 取回全文）
- **prefix hash**：折叠发生在 turn 边界，折叠后前缀 hash 变化一次——providers 缓存失效一次是可接受的（与 RestoreHistory 同理）

### 3.4 里程碑拆分

- **P2b-M6**：阈值检测 + turn 边界折叠 + compaction 记录落盘 + 模板摘要 + 测试
- **P2b-M7**：--resume 重建应用折叠 + 测试
- **P2b-M8**：模型自摘要——**暂缓**（已定：模板摘要先行）

### 3.5 已定的问题（2026-11-10）

7. **是。** `compact_at` 默认 0.7；分母取自第 5 节的 `context_tokens`，不再用 `max_tokens`。
8. **是。** 模板摘要先行，模型自摘要（M8）暂缓。
9. **A。** compaction 默认开（`compact_at: 0.7`，显式置 0 关闭）。

---

## 4. 跨会话记忆

### 4.1 数据结构

```markdown
---
kind: memory
id: mem-<date>-<rand>
scope: project | user          # project 绑定工作目录哈希；user 全局
tags: [owltide, go]
created: RFC3339
updated: RFC3339
source: session:<id>           # 产生该记忆的会话
---
<一句话记忆正文（<= 2 句）>
```

- 记忆文件：`~/.owltide/memories/<scope>/<id>.md`（目录已存在于 runtime state，当前为空壳）
- 注入：系统提示尾部追加「Relevant memories」块。M0 全量注入最新 20 条（总长度设上限，超限按截断保留最新）；超过 20 条时才做词面 top-K（按 tags 与当前任务词重叠度，无向量）。注入内容进前缀 hash，故**每会话只注入一次**（会话开始时定型，中途不换）

### 4.2 写入位置

- 工具 `memory_save`（主代理 registry，白名单进 router AllowedTools）：正文 + tags 必填，scope 默认 project；写入前进第 6 节权限服务的待审批队列，可选「本会话自动同意」
- 工具 `memory_forget`（按 id，soft delete → status: archived frontmatter 字段）
- 去重：同 scope 下正文 sha256 相同则拒绝（幂等）

### 4.3 与现有机制的关系

- **evolve/store**：memories/ 在 store 内，auto-commit 覆盖，`owltide changes` 可见（用户可审查模型记了什么——这是刻意的透明性）
- **sessionlog**：memory_save 调用本身作为普通工具记录进主日志
- **guard**：memories/ 加入 protected files（run_shell 不可绕过工具直接改）

### 4.4 里程碑拆分

- **P2b-M9**：memory_save/forget + 注入（词面 top-K）+ 去重 + guard 保护 + 测试
- **P2b-M10**：`owltide memories` 列表/清理命令——未排期（本轮未答复，随 M9 一起再议）

### 4.5 已定的问题（2026-11-10）

10. **B。** 全量注入最新 20 条，总长度设上限；超过 20 条再做词面 top-K。
11. **是。** `memory_save` 走第 6 节权限服务的待审批队列，可选「本会话自动同意」。
12. **是。** 子代理只读 memories，主代理汇总后写。

---

## 5. 模型参数自动匹配（已定：纳入，排在压缩之前——压缩阈值依赖它）

### 5.1 动机

config.yaml 只配 model.name + base_url；context window / max output / 图像 / 工具调用能力全靠用户手填或猜。配错的表现：上下文截断静默丢历史、max_tokens 超限报错、不支持 tool call 的模型死循环。

### 5.2 查找顺序（四级回退）

```
1. 用户配置        config.yaml model.limits.{context_tokens,max_output,images,tools}
2. 端点 /models    GET {base_url}/models → 元数据（若端点返回 context_window/max_output 等字段）
3. 内置表          内置常见模型族的已知参数（编译进二进制的小表，键为模型名前缀匹配）
4. 保守默认值      context 32768 / max_output 4096 / images=false / tools=true
```

- 用户配置命中 / 内置表命中：启动日志打印一行来源（不落盘、不进提交）
- /models 探测结果按模型名缓存（进程内，含失败负缓存）；探测结果**不进日志、不进提交**
- 探测默认开启（已定）；失败（网络/不支持）静默降级到下一级，不阻塞启动

### 5.3 数据结构

```go
// core/config/modellimits.go
type ModelLimits struct {
    ContextTokens int   `yaml:"context_tokens"`
    MaxOutput     int   `yaml:"max_output"`
    Images        bool  `yaml:"images"`
    Tools         bool  `yaml:"tools"`
    Source        string `yaml:"-"` // user | endpoint | builtin | conservative
}
```

- 用途：budget 计算按比例取 context（compaction 阈值、token budget 上限校验）；images=false 时 multimodal 工具不注册；tools=false 启动即报错（当前架构强依赖 tool call）

### 5.4 与现有机制的关系

- **config**：Model struct 加 Limits 字段；Load 时按四级回退解析一次（用户配置始终优先）
- **compaction（第 3 节）**：阈值基于 ContextTokens；解析顺序排在压缩之前
- **内置表**：小表（仅常见模型族），每项注明来源与录入日期，会随时间过期——用户配置始终优先
- 不引入网络依赖的新库：/models 用现有 model.Client 的 HTTP 栈

### 5.5 里程碑拆分

- **P2b-M11**：ModelLimits 结构 + 四级回退 + 探测缓存 + 测试（httptest 模拟 /models）
- **P2b-M12**：budget/compaction 接入 ContextTokens + tools=false 快速失败

### 5.6 已定的问题（2026-11-10）

13. **是。** 纳入 P2b 范围，且排在压缩之前做（压缩阈值依赖 context_tokens）。
14. **是。** 内置表接受，但要小：仅常见模型族，每项注明来源与录入日期；用户配置始终优先。
15. **否。** 不需要显式开启——默认探测，按模型名缓存，失败静默回退；探测结果不进日志和提交。

---

## 6. 权限服务

状态：已定，执行顺序第 2（先于所有"写"里程碑，也先于 serve 写接口与 UI 接入）

### 6.1 定位

一个独立包（CLI、serve、将来的 TUI 共用），一张表统一管理所有"等用户"的状态：越权审批、ask_user、计划批准、memory_save。任何端（终端、SSE 客户端、未来的 TUI）都通过同一张表领取和回答请求——不允许每个入口各有一套"等用户"逻辑。

### 6.2 数据结构

```go
// internal/perm（包名暂定）
type Kind string // approval | ask_user | plan_approval | memory_save

const (
    KindApproval     Kind = "approval"      // 越权/危险动作审批（含 router 越权的放行请求）
    KindAskUser      Kind = "ask_user"      // 主代理提问
    KindPlanApproval Kind = "plan_approval" // 计划批准
    KindMemorySave   Kind = "memory_save"   // 跨会话记忆写入
)

type Request struct {
    ID      string    // 递增 id：req-<seq>
    Kind    Kind
    Source  Source    // 来源
    Payload any       // 类型相关的请求体（命令 argv、问题文本、plan id、memory 正文）
    Created time.Time
    State   State     // pending | approved | denied | timeout | cancelled
    Answered time.Time
    By      string    // 回答端标识（tty / serve:<client> / auto-session）
}

type Source struct {
    Agent   string // "main" 或子代理 id（sub-xxxx）
    Role    string // 角色名（general/explore/…）
    Session string
}

type State string // pending | approved | denied | timeout | cancelled
```

- 请求只存内存，进程退出即清空（跨会话记忆针对的是记忆本身，不是待审批队列）
- 终态四类：**同意、拒绝、超时、已取消**（用户显式取消或会话结束）

### 6.3 并发与"本会话允许"

- 多端同时在线：**先到先得**。第一个回答端把请求置终态；后到的端收到"已处理"（含终态与回答端标识），不能重复决定。
- "本会话允许"（session allow）：按**具体命令**记录（精确 argv 相等才匹配），可列出（`owltide allow --list` 或 SSE 事件）、可撤销；进程/会话结束即失效。
- **不做**按类别（如"所有 shell 命令"）或跨会话的永久授权；危险请求（删除、强推、受保护文件写入）超时默认**拒绝**，非危险请求超时默认拒绝同样适用（一律默认拒绝，安全侧兜底）。
- 社交入口（Channel 插件）来的请求一律按只读处理：**不能从群里触发写文件或 shell**——社交入口只能 ask_user 式的纯文本回答，写类审批在社交入口直接以"该操作不允许从群聊发起"拒绝。

### 6.4 与现有机制的关系

- **ask_user**：现有 `agent.AskUserTool` 迁到权限服务上（KindAskUser）；交互终端、serve SSE、未来 TUI 都是"端"，同一请求只有一个答案。
- **guard / confirmFromStdin**：现有 `tool.Confirmer` 接口保留，实现切到权限服务（KindApproval）——危险动作的确认不再绑定 stdin。
- **subagent**：子代理请求审批时，Source 带子代理 id 与角色（审计可追溯）；回答后结果经现有 tool 结果通道回到子代理。
- **decisions**：每个终态写一条 `{event: "permission", id, kind, state, by, source}` 审计事件，供对账。
- **serve**：审批类事件走 SSE 必达通道（见 `serve-api.md`），不进"慢客户端丢帧"路径。
- **evolve**：`allowlist`（本会话允许的列表本身）不写盘、不进 store，避免模型自我扩权。

### 6.5 里程碑拆分

- **P2b-M13**：包结构 + 表 + 状态机 + 先到先得 + 超时默认拒绝 + decisions 事件 + 测试
- **P2b-M14**：ask_user / guard confirmer 迁移到权限服务（行为兼容）+ 测试
- **P2b-M15**：session allow（按命令、列出、撤销）+ 社交入口只读限制 + 测试
- **P2b-M16**：serve SSE 审批必达通道 + Last-Event-ID 补发接线（与 serve-api.md 同步）+ 测试

### 6.6 已定的问题

无待确认事项（本轮已定）。

---

## 附：里程碑总览（2026-11-10 更新执行顺序）

| 执行顺序 | 里程碑 | 内容 | 依赖 |
|----------|--------|------|------|
| 1 | M11/M12 | 模型参数自动匹配（原排最后，提前：压缩阈值依赖 context_tokens） | — |
| 2 | 权限服务 | 第 6 节，新（包名暂定 core/perm 或 internal/perm，随实现定） | — |
| 3 | M1、M2 | goal/plan/todo 格式+工具+guard；会话恢复注入 + context_refs（M3 的 `owltide goals` 命令可延后） | 权限服务先于写工具类里程碑 |
| 4 | M6、M7 | compaction 折叠+落盘；resume 应用折叠（M8 自摘要暂缓） | M11/M12 |
| 5 | M4、M5 | verifier 角色；verified 事件 + done 联动（软提示） | M1 |
| 6 | M9 | 跨会话记忆（memory_save 走权限服务） | 权限服务 |

- 约束：**权限服务完成后再做 serve 写接口和 UI 接入**（写接口的审批必须走权限服务，不另起炉灶）。
- 未排期：M3 `owltide goals` 命令（延后）、M8 模型自摘要（暂缓）、M10 `owltide memories` 命令（原可选，未排期）。
